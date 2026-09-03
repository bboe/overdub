package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bboe/overdub/internal/audio"
	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
)

const (
	inputNode  = "/dev/input/event1"
	actionKey  = 138
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

	i, err := button.Open(inputNode, actionKey, uinputName, nodeWait)
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

	log.Printf("intercepting %s: consuming keycode %d, passing the rest to %q",
		inputNode, actionKey, uinputName)

	// The chime sounds at the key-down and the event is sent at the release,
	// because they answer different questions. The chime says the daemon has the
	// button, and it is worth nothing if it waits for a hold to end; the event
	// says what the press was, which nothing knows until the key comes up.
	return i.Run(
		func() {
			if chime == nil {
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
		func(held time.Duration) {
			event := pressEvent(held)
			log.Printf("intercepted %d: %s (held %v)", actionKey, event, held.Round(time.Millisecond))
			// Sent on the read loop rather than through a queue: FirePress holds
			// the server lock only long enough to queue a frame per subscriber,
			// and every other holder of that lock is as short. A goroutine each
			// would be the one thing worse than blocking here, since two presses
			// could then reach Home Assistant in the wrong order.
			if server := api.Load(); server != nil {
				server.FirePress(event)
			}
		})
}

// Which of the two the button did. Held at the threshold counts as a hold, so
// the boundary belongs to the longer press: a user holding for exactly as long
// as they were told to gets the hold rather than a press.
func pressEvent(held time.Duration) esphome.EventType {
	if held >= holdTime {
		return esphome.EventHold
	}
	return esphome.EventPress
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
	// The button owns whether it is captured; the server reads it and asks for
	// it to change. Wired here rather than passed to NewServer because a server
	// is testable without a button and a Dot never runs without one.
	server.UseButton(i.Captured, i.SetCaptured)
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
