package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bboe/overdub/internal/alexa"
	"github.com/bboe/overdub/internal/audio"
	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
	"github.com/bboe/overdub/internal/mdns"
	"github.com/bboe/overdub/internal/sendspin"
	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	inputNode  = "/dev/input/event1"
	actionKey  = 138
	muteKey    = 113
	uinputName = "mtk-kpd"
	volumeName = "overdub-volume"
	wifiIface  = device.WifiInterface
	apiPort    = 6053

	deviceModel = "Echo Dot (2nd Generation)"

	noiseKeyPath    = "/data/local/bin/.overdub-noise-key"
	sendspinKeyPath = "/data/local/bin/.overdub-sendspin-key"

	sendspinBuffer = 500
	sendspinLead   = 350

	nodeWait    = 60 * time.Second
	addressWait = 5 * time.Minute
	macWait     = 60 * time.Second
	macRetry    = 30 * time.Second
	sensorTick  = 60 * time.Second
	liveTick    = 500 * time.Millisecond
	firewallRe  = 30 * time.Second

	holdTime = 600 * time.Millisecond

	playbackWait = 30 * time.Second

	alexaWait  = 30 * time.Second
	alexaTries = 10

	commandRetry = 5 * time.Minute

	multiGap = 350 * time.Millisecond
)

var advertised atomic.Pointer[mdns.Responder]

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

	var player sendspin.Player
	if chime != nil {
		player = chimePlayer{chime: chime}
	}

	go serveAPI(flags.Name, psk, i, volume, player)

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

func serveAPI(name string, psk []byte, i *button.Interceptor, volume *button.VolumeKeys,
	player sendspin.Player) {
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

	base := []mdns.Advert{esphome.Advert(name, mac, apiPort)}
	keys, haveKeys := sendspinKeys()
	wantSendspin, known, err := device.Flag(sendspinFlag)
	switch {
	case err != nil:
		log.Printf("sendspin: %v; staying off until the switch says otherwise", err)
		wantSendspin = false
	case !known:
		wantSendspin = true
	}

	responder := &mdns.Responder{Instance: name, Iface: wifiIface, Services: base}
	advertised.Store(responder)
	go responder.Run()

	up := false
	if haveKeys {
		toggle := &sendspinSwitch{
			name: name, mac: mac, keys: keys, player: player,
			responder: responder, base: base,
			wake: server.NoteSendspin,
			peer: &untrustedlog.Log{Subject: "sendspin"},
			want: make(chan bool, 1),
		}
		go toggle.run()
		if wantSendspin {
			toggle.enable()
			up = toggle.On()
		} else if known {
			log.Printf("sendspin: switched off at the last restart, so it stays off")
		}
		server.UseSendspin(toggle.On, toggle.Set)
	}
	if !up {
		go sweepSendspinRule()
	}

	if err := device.AllowTCP(apiPort); err != nil {
		log.Printf("firewall: %v", err)
	}
	go device.HoldTCPOpen(apiPort, firewallRe, nil)
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

const sendspinFlag = "sendspin"

type advertiser interface {
	Advertise([]mdns.Advert) error
}

type sendspinServer interface {
	Serve(net.Listener) error
	Close()
}

type chimePlayer struct{ chime *audio.Chime }

func (p chimePlayer) OpenStream(say func(string, ...any)) (sendspin.Stream, error) {
	stream, err := p.chime.OpenStream(say)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

type sendspinSwitch struct {
	name, mac string
	keys      sendspin.Keys
	player    sendspin.Player
	responder advertiser
	base      []mdns.Advert
	wake      func()
	deny      func(int) error
	allow     func(int) error
	ready     func(<-chan struct{}) bool
	peer      *untrustedlog.Log

	want chan bool

	mu      sync.Mutex
	on      bool
	working bool
	client  sendspinServer
	ln      net.Listener
	hold    chan struct{}
	held    sync.WaitGroup
}

func (s *sendspinSwitch) On() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

func (s *sendspinSwitch) Set(on bool) {
	select {
	case s.want <- on:
	default:
		select {
		case <-s.want:
		default:
		}
		select {
		case s.want <- on:
		default:
		}
	}
}

func (s *sendspinSwitch) run() {
	for on := range s.want {
		accepted := false
		if on {
			accepted = s.enable()
		} else {
			accepted = s.disable()
		}
		if !accepted {
			continue
		}
		if err := device.SetFlag(sendspinFlag, on); err != nil {
			log.Printf("sendspin: %v; the switch holds until the next restart only", err)
		}
		if s.wake != nil {
			s.wake()
		}
	}
}

func sweepSendspinRule() {
	if err := device.DenyTCP(sendspin.Port); err != nil {
		log.Printf("firewall: %v; a rule for tcp/%d may be left from an earlier run",
			err, sendspin.Port)
	}
}

func (s *sendspinSwitch) begin(want bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.working || s.on == want {
		return false
	}
	s.working = true
	return true
}

func (s *sendspinSwitch) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.working = false
}

func (s *sendspinSwitch) enable() bool {
	if !s.begin(true) {
		return false
	}
	defer s.end()

	client := sendspinClient(s.name, s.mac, s.keys, s.player, s.peer)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", sendspin.Port))
	if err != nil {
		log.Printf("sendspin: %v; the dot will not join a music assistant group", err)
		return true
	}
	if err := device.AllowTCP(sendspin.Port); err != nil {
		log.Printf("firewall: %v", err)
	}
	hold := make(chan struct{})
	s.held.Add(1)
	go func() {
		defer s.held.Done()
		device.HoldTCPOpen(sendspin.Port, firewallRe, hold)
	}()

	s.mu.Lock()
	s.on, s.client, s.ln, s.hold = true, client, ln, hold
	s.mu.Unlock()

	go func() { s.peer.Printf("sendspin stopped: %v", client.Serve(ln)) }()
	s.held.Add(1)
	go func() {
		defer s.held.Done()
		s.advertiseOnceReachable(hold)
	}()
	s.peer.Printf("sendspin: switched on")
	return true
}

func (s *sendspinSwitch) allowRule() error {
	if s.allow != nil {
		return s.allow(sendspin.Port)
	}
	return device.AllowTCP(sendspin.Port)
}

func (s *sendspinSwitch) waitReachable(stop <-chan struct{}) bool {
	if s.ready != nil {
		return s.ready(stop)
	}
	return device.WaitForIPv4(wifiIface, addressWait, stop)
}

func stopped(hold <-chan struct{}) bool {
	select {
	case <-hold:
		return true
	default:
		return false
	}
}

func (s *sendspinSwitch) advertiseOnceReachable(hold <-chan struct{}) {
	if !s.waitReachable(hold) {
		if stopped(hold) {
			return
		}
		log.Printf("sendspin: %s still has no address; advertising anyway", wifiIface)
	}
	if stopped(hold) {
		return
	}
	if err := s.allowRule(); err != nil {
		log.Printf("firewall: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.on {
		return
	}
	if err := s.responder.Advertise(append(append([]mdns.Advert{}, s.base...),
		sendspin.Advert(s.name))); err != nil {
		log.Printf("mdns: %v; sendspin answers queries but was not announced", err)
	}
}

func (s *sendspinSwitch) disable() bool {
	if !s.begin(false) {
		return false
	}
	defer s.end()

	s.mu.Lock()
	client, ln, hold := s.client, s.ln, s.hold
	s.on, s.client, s.ln, s.hold = false, nil, nil, nil
	s.mu.Unlock()

	if err := s.responder.Advertise(append([]mdns.Advert{}, s.base...)); err != nil {
		log.Printf("mdns: %v; sendspin was not withdrawn cleanly", err)
	}
	close(hold)
	s.held.Wait()
	ln.Close()
	client.Close()
	if err := s.denyRule(); err != nil {
		log.Printf("firewall: %v; tcp/%d stays open with nothing behind it until netd"+
			" rebuilds the chain", err, sendspin.Port)
	}
	s.peer.Printf("sendspin: switched off")
	return true
}

func (s *sendspinSwitch) denyRule() error {
	if s.deny != nil {
		return s.deny(sendspin.Port)
	}
	return device.DenyTCP(sendspin.Port)
}

func sendspinKeys() (sendspin.Keys, bool) {
	keys, created, err := sendspin.LoadOrCreateKeys(sendspinKeyPath)
	if err != nil {
		log.Printf("sendspin: %v; the dot will not join a music assistant group", err)
		return sendspin.Keys{}, false
	}
	if created {
		log.Printf("sendspin: this dot had no identity, so one was generated at %s",
			sendspinKeyPath)
	}
	log.Printf("sendspin: client %s; docs/sendspin.md says how to read the pairing token",
		keys.Identity.ClientID())
	return keys, true
}

func sendspinClient(name, mac string, keys sendspin.Keys, player sendspin.Player,
	peer *untrustedlog.Log) *sendspin.Client {
	return &sendspin.Client{
		Config: sendspin.Config{
			Name:           name,
			ProductName:    deviceModel,
			Manufacturer:   "Amazon",
			MACAddress:     strings.ToLower(mac),
			UnpairedAccess: true,
			BufferCapacity: sendspin.BufferCapacity,
		},
		Keys:           keys,
		PSKs:           sendspin.PSKSet{Pairing: keys.PairingPSK},
		MinBufferMS:    sendspinBuffer,
		RequiredLeadMS: sendspinLead,
		Player:         player,
		Peer:           peer,
	}
}
