package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bboe/overdub/internal/alexa"
	"github.com/bboe/overdub/internal/audio"
	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
	"github.com/bboe/overdub/internal/mdns"
)

const (
	inputNode  = "/dev/input/event1"
	actionKey  = 138
	muteKey    = 113
	uinputName = "mtk-kpd"
	volumeName = "overdub-volume"
	wifiIface  = device.WifiInterface
	apiPort    = 6053

	noiseKeyPath = "/data/local/bin/.overdub-noise-key"

	nodeWait   = 60 * time.Second
	macWait    = 60 * time.Second
	macRetry   = 30 * time.Second
	sensorTick = 60 * time.Second
	liveTick   = 500 * time.Millisecond
	firewallRe = 30 * time.Second

	holdTime = 600 * time.Millisecond

	playbackWait = 30 * time.Second

	alexaWait  = 30 * time.Second
	alexaTries = 10

	commandRetry = 5 * time.Minute

	multiGap = 350 * time.Millisecond
)

var advertised atomic.Pointer[mdns.Responder]

const deviceModel = "Echo Dot (2nd Generation)"

var api atomic.Pointer[esphome.Server]

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

	chime, err := audio.NewChime()
	if err != nil {
		log.Printf("warning: %v; presses will be silent", err)
		chime = nil
	} else {
		defer chime.Close()
	}

	volume, err := button.NewVolumeKeys(volumeName)
	if err != nil {
		log.Printf("warning: %v; the volume cannot be set from home assistant", err)
		volume = nil
	} else {
		defer volume.Close()
	}

	go serveAPI(flags.Name, psk, i, volume)

	var held []string
	for code, b := range buttons {
		held = append(held, fmt.Sprintf("%d %s (%v)", code, b.objectID, b.start))
	}
	sort.Strings(held)
	log.Printf("watching %s: %s, passing the rest to %q",
		inputNode, strings.Join(held, ", "), uinputName)

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
				if server := api.Load(); server != nil {
					server.FirePress(objectID, event, count, holdFor)
				}
			})
	}

	return i.Run(
		func(code uint16, mode button.Mode) {
			chains[code].Down()
			if chime == nil || !chimes(code, mode) {
				return
			}
			go func() {
				if err := chime.Play(); err != nil {
					log.Printf("chime: %v", err)
				}
			}()
		},
		func(code uint16, _ button.Mode, held time.Duration) { chains[code].Up(held) })
}

var buttons = map[uint16]struct {
	objectID string
	start    button.Mode
	chime    bool
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

func chimes(code uint16, mode button.Mode) bool {
	return mode == button.ModeIntercept && buttons[code].chime
}

func parseButtonMode(choice string) (button.Mode, bool) {
	for _, m := range []button.Mode{button.ModeIntercept, button.ModeMonitor, button.ModePassThrough} {
		if m.String() == choice {
			return m, true
		}
	}
	return button.ModeIntercept, false
}

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

func serveAPI(name string, psk []byte, i *button.Interceptor, volume *button.VolumeKeys) {
	mac := device.WaitForMAC(wifiIface, macWait)
	if mac == "" {
		log.Printf("%s has no address yet; the button works, and the api starts if it appears", wifiIface)
		for mac == "" {
			time.Sleep(macRetry)
			mac = device.MACAddress(wifiIface)
		}
		log.Printf("%s appeared", wifiIface)
	}
	server := esphome.NewServer(name, deviceModel, version, mac, psk)
	for code, b := range buttons {
		server.UseButton(b.objectID,
			func() string { return i.Mode(code).String() },
			func(choice string) {
				if m, ok := parseButtonMode(choice); ok {
					i.SetMode(code, m)
				}
			})
	}
	server.UseMicMute(func() error {
		err := i.Press(muteKey)
		if errors.Is(err, button.ErrKeyStuck) {
			log.Printf("microphone: %v; exiting so the clone is rebuilt", err)
			withdraw()
			i.Close()
			os.Exit(1)
		}
		return err
	})

	if volume != nil {
		server.UseVolumeKeys(func(up bool, n int) error {
			err := volume.Step(up, n)
			if errors.Is(err, button.ErrKeyStuck) {
				log.Printf("volume: %v; exiting so the device is rebuilt", err)
				withdraw()
				i.Close()
				os.Exit(1)
			}
			return err
		})
	}

	switch jar, registered, known := commandState(); {
	case commandReady(jar, registered, known):
		server.UseCommand(alexa.NewClient().Send)
	case !jar:
		log.Printf("alexa: no %s, so nothing here can run a command and the api offers no "+
			"command box; asking again every %v", alexa.JarPath, commandRetry)
		go waitForCommand(server)
	default:
		log.Printf("alexa: this dot holds no amazon account, so a command would have no "+
			"credential to run with and the api offers no command box; asking again every %v",
			commandRetry)
		go waitForCommand(server)
	}

	go waitForAlexa(server)

	api.Store(server)

	responder := &mdns.Responder{
		Instance: name,
		Iface:    wifiIface,
		Services: []mdns.Advert{esphome.Advert(name, mac, apiPort)},
	}
	advertised.Store(responder)
	go responder.Run()

	if err := device.AllowTCP(apiPort); err != nil {
		log.Printf("firewall: %v", err)
	}
	go device.HoldTCPOpen(apiPort, firewallRe)
	server.Poll(sensorTick, liveTick)

	log.Printf("esphome api stopped: %v", server.Listen(fmt.Sprintf(":%d", apiPort)))
	withdraw()
	os.Exit(1)
}

func commandState() (jar, registered, known bool) {
	if !alexa.JarInstalled() {
		return false, false, false
	}
	registered, known = device.AlexaRegistered()
	return true, registered, known
}

func waitForCommand(server *esphome.Server) {
	for {
		time.Sleep(commandRetry)
		if commandReady(commandState()) {
			server.UseCommand(alexa.NewClient().Send)
			log.Printf("alexa: a jar and an account are both here now; the command box is offered")
			return
		}
	}
}

func commandReady(jar, registered, known bool) bool {
	if !jar {
		return false
	}
	return registered || !known
}

func waitForAlexa(server *esphome.Server) {
	for attempt := 1; ; attempt++ {
		if alexa.Installed() {
			usePlayback(server)
			if attempt > 1 {
				log.Printf("alexa: %s answered on attempt %d; the media player can play now",
					alexa.Package, attempt)
			}
			return
		}
		if attempt == 1 {
			log.Printf("alexa: %s is not installed yet, so the media player offers the volume "+
				"alone; asking again for %v", alexa.Package, alexaWait*(alexaTries-1))
		}
		if attempt >= alexaTries {
			log.Printf("alexa: %s never answered; nothing here can play a clip until a restart",
				alexa.Package)
			return
		}
		time.Sleep(alexaWait)
	}
}

func usePlayback(server *esphome.Server) {
	watcher := &alexa.PlaybackWatcher{
		OnStart: func() { server.NotePlayback(true) },
		OnEnd: func(ok bool, detail string) {
			if ok {
				server.NotePlayback(false)
				return
			}
			server.NotePlaybackFailed(detail)
		},
	}
	go watcher.Run()
	server.UsePlay(func(url string) error {
		watcher.Expect(playbackWait)
		if err := alexa.Speak(url); err != nil {
			watcher.Cancel()
			return err
		}
		watcher.Extend(playbackWait)
		return nil
	})
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
