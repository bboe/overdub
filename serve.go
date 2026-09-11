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

	"github.com/bboe/overdub/internal/audio"
	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
)

const (
	inputNode  = "/dev/input/event1"
	actionKey  = 138
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

	holdTime = 600 * time.Millisecond

	multiGap = 350 * time.Millisecond
)

var advertised atomic.Pointer[esphome.Responder]

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

	go serveAPI(flags.Name, psk, i)

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

func serveAPI(name string, psk []byte, i *button.Interceptor) {
	mac := device.WaitForMAC(wifiIface, macWait)
	if mac == "" {
		log.Printf("%s has no address yet; the button works, and the api starts if it appears", wifiIface)
		for mac == "" {
			time.Sleep(macRetry)
			mac = device.MACAddress(wifiIface)
		}
		log.Printf("%s appeared", wifiIface)
	}
	server := esphome.NewServer(name, "Echo Dot (2nd Generation)", mac, psk)
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

	api.Store(server)

	responder := &esphome.Responder{Instance: name, MAC: mac, Iface: wifiIface, Port: apiPort}
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
