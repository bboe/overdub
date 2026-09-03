package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bboe/overdub/internal/audio"
	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
)

const (
	inputNode = "/dev/input/event1"
	actionKey = 138
	// The microphone mute, on the same node as the action button. Alexa answers
	// it, so it ships in monitor: taking it by default would leave a Dot that
	// cannot be muted by the button that says mute on it.
	muteKey    = 113
	uinputName = "mtk-kpd"
	wifiIface  = device.WifiInterface
	apiPort    = 6053

	noiseKeyPath = "/data/local/bin/.overdub-noise-key"

	nodeWait   = 60 * time.Second
	macWait    = 60 * time.Second
	macRetry   = 30 * time.Second
	sensorTick = 60 * time.Second
	liveTick   = 500 * time.Millisecond
	firewallRe = 30 * time.Second

	// What separates a press from a hold, and it is Alexa's own number rather
	// than one chosen here: a BUTTON_MODE she sees held past this is her long
	// press, which is setup mode. So the hold a user already has in their hand
	// is the hold this reports.
	holdTime = 600 * time.Millisecond

	// What separates one run from the next, and so what a single press waits
	// before it is reported at all. docs/architecture.md says why.
	multiGap = 350 * time.Millisecond
)

// The responder exists only once serveAPI has an address to advertise, and the
// signal handler is installed before that. Held here so the handler can withdraw
// the records rather than leave Home Assistant to time them out.
var advertised atomic.Pointer[esphome.Responder]

// The API server exists only once serveAPI has the MAC that names it, and the
// read loop is running well before that: the button is taken first, so a Dot
// with no wlan0 keeps its button. A press with nobody to tell is therefore
// dropped rather than queued, which is what an event means -- one delivered
// when the network finally came up would say the button was pressed at a moment
// it was not.
var api atomic.Pointer[esphome.Server]

// Every path out of this process goes through here, because an advertised
// record that outlives the daemon is one Home Assistant keeps for the PTR's
// full lifetime. The listener dying is the likelier exit and the one where the
// record has actually become a lie. The button failing is the other, and it
// returns through main rather than exiting here, which is why main withdraws
// too: the read loop and a failed re-emission are both fatal by design.
func withdraw() {
	if r := advertised.Load(); r != nil {
		r.Goodbye()
	}
}

func serve(flags config) error {
	psk, err := loadPSK(noiseKeyPath)
	if err != nil {
		return err
	}

	i, err := button.Open(inputNode, uinputName, nodeWait, buttonStart())
	if err != nil {
		return err
	}
	defer i.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		withdraw()
		i.Close()
		os.Exit(0)
	}()

	// Held open for the life of the daemon. Building the player per press was
	// measured at 333ms of process startup and dynamic linking against 33ms
	// once it is up, and there is nothing to serve or supervise either way.
	chime, err := audio.NewChime()
	if err != nil {
		log.Printf("warning: %v; presses will be silent", err)
		chime = nil
	} else {
		defer chime.Close()
	}

	// Off the read loop, because the button is not the network's to wait for: a
	// Dot with no wlan0 keeps its button rather than restarting every five
	// seconds, and mute passes through while this is still waiting.
	go serveAPI(flags.Name, psk, i)

	// What the daemon is holding, and in which mode, because the modes move
	// afterwards and this is the only startup record of where they began.
	var held []string
	for code, b := range buttons {
		held = append(held, fmt.Sprintf("%d %s (%v)", code, b.objectID, b.start))
	}
	sort.Strings(held)
	log.Printf("watching %s: %s, passing the rest to %q",
		inputNode, strings.Join(held, ", "), uinputName)

	// One chain per button, because a run belongs to the key it was pressed on:
	// sharing one would make a press of each look like a double press of
	// either. Called from whichever goroutine recognised the gesture, and
	// MultiPress orders them per chain, so two gestures of one button cannot
	// reach Home Assistant the wrong way round.
	//
	// The line does not say which mode the press was in, and the word is not
	// "intercepted": monitor reports a press it also handed to Alexa. Which
	// mode is in force is logged where it changes, which is once rather than
	// once a press.
	chains := map[uint16]*button.MultiPress{}
	for code, b := range buttons {
		objectID := b.objectID
		chains[code] = button.NewMultiPress(multiGap, holdTime,
			func(g button.Gesture, count int, holdFor time.Duration) {
				event, ok := pressEvent(g)
				if !ok {
					log.Printf("%s %d: gesture %v has no esphome name; not reported", objectID, code, g)
					return
				}
				switch {
				case holdFor > 0:
					log.Printf("%s %d: %s (held %v)", objectID, code, event, holdFor.Round(time.Millisecond))
				case count > 0:
					log.Printf("%s %d: %s (%d)", objectID, code, event, count)
				default:
					log.Printf("%s %d: %s", objectID, code, event)
				}
				// Sent from here rather than through a queue: FirePress holds
				// the server lock only long enough to queue a frame per
				// subscriber. A queue that dropped what it could not take would
				// lose presses to report.
				if server := api.Load(); server != nil {
					server.FirePress(objectID, event, count, holdFor)
				}
			})
	}

	// The chime is at the key-down, before anything knows what the run will be,
	// so it sounds once per press rather than once per gesture.
	return i.Run(
		func(code uint16, mode button.Mode) {
			chains[code].Down()
			if chime == nil || !chimes(code, mode) {
				return
			}
			// Off the read loop, which mute passes through. Play only queues the
			// clip rather than waiting for it, but it still takes the player's
			// lock and a few milliseconds, and a press restarts a chime already
			// sounding rather than being dropped.
			go func() {
				if err := chime.Play(); err != nil {
					log.Printf("chime: %v", err)
				}
			}()
		},
		func(code uint16, _ button.Mode, held time.Duration) { chains[code].Up(held) })
}

// The keys this daemon acts on: what each reports as, and what it starts in.
// One map rather than two, so a key cannot be watched without an entity to
// report it, or given an entity nothing watches. The object ids are
// internal/esphome's, and a name here it does not have reports nothing.
var buttons = map[uint16]struct {
	objectID string
	start    button.Mode
	// Whether an intercepted press of this key sounds the chime. Only the
	// action button does. The chime reads as an acknowledgement, and on a key
	// whose job is to cut the microphone that is the one thing it must not
	// say: an intercepted mute leaves the mic live and the ring dark, so a
	// sound there tells somebody they are muted when they are not. Silence is
	// the honest signal -- the absence of Alexa's own tone is what says the
	// key did not do what is written on it.
	chime bool
}{
	actionKey: {"action_button", button.ModeIntercept, true},
	muteKey:   {"mute_button", button.ModeMonitor, false},
}

func buttonStart() map[uint16]button.Mode {
	out := map[uint16]button.Mode{}
	for code, b := range buttons {
		out[code] = b.start
	}
	return out
}

// Whether a press sounds the chime. Two things have to hold. The daemon must be
// keeping the key: in monitor Alexa answers the same press herself and two
// acknowledgements for one press is worse than none, and in pass through there
// is nothing of ours to acknowledge. And the key must be one an acknowledgement
// is true of, which is the action button and not the mute -- see buttons.
func chimes(code uint16, mode button.Mode) bool {
	return mode == button.ModeIntercept && buttons[code].chime
}

// The modes the button has, under the names Home Assistant is offered. Two
// functions rather than one map so that a mode with no name fails to compile
// here rather than reaching a listing that cannot name it.
func parseButtonMode(choice string) (button.Mode, bool) {
	for _, m := range []button.Mode{button.ModeIntercept, button.ModeMonitor, button.ModePassThrough} {
		if m.String() == choice {
			return m, true
		}
	}
	return button.ModeIntercept, false
}

// What the gesture is called on the wire, and the whole of the translation.
// listEntities advertises exactly these, and Home Assistant drops an event it
// was not told about.
//
// A gesture with no name here is reported as nothing rather than as the nearest
// one. A default that answered press_end would report a single press for a
// gesture added later and never noticed, which is the wrong reading rather than
// no reading.
func pressEvent(g button.Gesture) (esphome.EventType, bool) {
	switch g {
	case button.GesturePressEnd:
		return esphome.EventPressEnd, true
	case button.GestureMultiEnd:
		return esphome.EventMultiEnd, true
	case button.GestureLongStart:
		return esphome.EventLongPressStart, true
	case button.GestureLongEnd:
		return esphome.EventLongPressEnd, true
	}
	return "", false
}

func serveAPI(name string, psk []byte, i *button.Interceptor) {
	mac := device.WaitForMAC(wifiIface, macWait)
	if mac == "" {
		// Kept waiting rather than given up on: nothing restarts a daemon that
		// did not exit, so giving up here would cost the API for the rest of the
		// boot on a Dot whose access point comes back a minute late. Quietly,
		// because the wait above has already said what it is waiting for.
		log.Printf("%s has no address yet; the button works, and the api starts if it appears", wifiIface)
		for mac == "" {
			time.Sleep(macRetry)
			mac = device.MACAddress(wifiIface)
		}
		log.Printf("%s appeared", wifiIface)
	}
	server := esphome.NewServer(name, "Echo Dot (2nd Generation)", mac, psk)
	// The button owns its mode; the server reads it and asks for it to change.
	// The names cross here rather than in either package, the way pressEvent
	// does. Wired here rather than passed to NewServer because a server is
	// testable without a button and a Dot never runs without one.
	for code, b := range buttons {
		server.UseButton(b.objectID,
			func() string { return i.Mode(code).String() },
			func(choice string) {
				if m, ok := parseButtonMode(choice); ok {
					i.SetMode(code, m)
				}
			})
	}
	// Found rather than handed over: the read loop has been running since before
	// there was an address to build this server with.
	api.Store(server)

	responder := &esphome.Responder{Instance: name, MAC: mac, Iface: wifiIface, Port: apiPort}
	advertised.Store(responder)
	go responder.Run()

	if err := device.AllowTCP(apiPort); err != nil {
		log.Printf("firewall: %v", err)
	}
	go device.HoldTCPOpen(apiPort, firewallRe)
	server.Poll(sensorTick, liveTick)

	// A bind that fails leaves a daemon serving the button and nothing else, and
	// the supervisor cannot tell: it only restarts a process that exited.
	log.Printf("esphome api stopped: %v", server.Listen(fmt.Sprintf(":%d", apiPort)))
	withdraw()
	os.Exit(1)
}

func loadPSK(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w (deploy/install.sh generates one)", err)
	}
	psk, err := esphome.DecodeNoisePSK(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return psk, nil
}
