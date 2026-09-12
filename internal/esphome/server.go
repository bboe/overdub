// Package esphome pretends to be an ESPHome device, so Home Assistant adopts
// the Echo Dot with its own first-party integration: no custom component and no
// MQTT. The API is encrypted, and the pre-shared key it needs is the one
// credential the Dot holds.
// docs/api.md has the measurements.
package esphome

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	maxConns  = 8
	sendQueue = 64

	pingAfter = 60 * time.Second
	idleWait  = pingAfter * 5 / 2

	clientKeepalive = 20 * time.Second

	minLiveReadGap = time.Second

	HeavyEvery = 5

	SoundEvery = 1

	MinSensorTick = 30 * time.Second

	commandQueue = 4
)

// Message ids from esphome/components/api/api.proto.
const (
	msgHelloRequest      = 1
	msgHelloResponse     = 2
	msgConnectRequest    = 3
	msgConnectResponse   = 4
	msgDisconnectRequest = 5
	msgDisconnectResp    = 6
	msgPingRequest       = 7
	msgPingResponse      = 8
	msgDeviceInfoRequest = 9
	msgDeviceInfoResp    = 10
	msgListEntitiesReq   = 11
	msgListBinarySensor  = 12
	msgListSensor        = 16
	msgListSwitch        = 17
	msgListEntitiesDone  = 19
	msgSubscribeStates   = 20
	msgBinarySensorState = 21
	msgSensorState       = 25
	msgSwitchState       = 26
	msgSubscribeLogs     = 28
	msgSwitchCommand     = 33
	msgSubscribeHAServ   = 34
	msgHomeassistantAct  = 35
	msgSubscribeHAStates = 38
	msgListService       = 41
	msgExecuteService    = 42
	msgListSelect        = 52
	msgSelectState       = 53
	msgSelectCommand     = 54
	msgListMediaPlayer   = 63
	msgMediaPlayerState  = 64
	msgMediaPlayerCmd    = 65
	msgListText          = 97
	msgTextState         = 98
	msgTextCommand       = 99
	msgListEvent         = 107
	msgEventState        = 108
)

type physicalButton struct {
	objectID string
	name     string
	keyEvent uint32
	keyMode  uint32

	mode    func() string
	setMode func(string)
}

func shipped(mode string) func() string { return func() string { return mode } }

func entityKey(objectID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(objectID))
	return h.Sum32()
}

type frame struct {
	msgType int
	payload []byte
}

type conn struct {
	sock   net.Conn
	rw     *noiseRW
	out    chan frame
	states bool // sent SubscribeStatesRequest

	services bool

	said string

	noted string
}

type Server struct {
	name  string
	model string
	mac   string

	psk []byte

	keyUptime  uint32
	keyWifi    uint32
	keyVolume  uint32
	keyCPU     uint32
	keyMemory  uint32
	keyJack    uint32
	keyJackOn  uint32
	keySound   uint32
	keySpeaker uint32
	keyMicMute uint32
	keyADB     uint32
	keyAlexa   uint32
	keyText    uint32
	keyCommand uint32

	buttons []*physicalButton

	adbWorking    bool
	adbHasPending bool
	adbPending    device.ADBMode

	adbSettled bool

	adbLive device.ADBMode

	micWorking    bool
	micHasPending bool
	micPending    bool
	micSettled    bool
	micLive       bool

	mpPlaying bool

	playWorking    bool
	playHasPending bool
	playWant       string

	volWorking    bool
	volHasPending bool
	volWant       volumeWant

	cmdWorking bool
	cmdQueue   []string

	soundOn     bool
	soundGap    time.Duration
	onDelay     time.Duration
	offDelay    time.Duration
	soundSince  time.Time
	soundLastOn time.Time
	soundSeen   time.Time

	uptime  func() (float32, bool)
	wifi    func() (float32, bool)
	volumes func() device.MusicVolume
	jack    func() (bool, bool)
	sound   func() (bool, bool)
	micMute func() (bool, bool)
	cpu     func() (float32, bool)
	alexa   func() (bool, bool)
	memory  func() (float32, bool)

	volumeKeys  func(up bool, n int) error
	play        func(url string) error
	command     func(text string) error
	micPress    func() error
	adbMode     func() (device.ADBMode, bool)
	adbSet      func(device.ADBMode) error
	adbSecureOK func() bool
	adbHold     func() error
	adbDeny     func() error

	handshakeWait time.Duration
	pingWait      time.Duration
	wakeGap       time.Duration
	adbSettle     time.Duration
	micSettle     time.Duration
	volumeSettle  time.Duration

	mu    sync.Mutex
	conns map[*conn]struct{}

	published map[uint32]reading

	liveWake   chan struct{}
	sensorWake chan struct{}

	untrustedLog untrustedlog.Log
}

func NewServer(name, model, mac string, psk []byte) *Server {
	return &Server{
		name:         name,
		model:        model,
		mac:          mac,
		psk:          psk,
		untrustedLog: untrustedlog.Log{Subject: "esphome api"},
		keyUptime:    entityKey("uptime"),
		keyWifi:      entityKey("wifi_signal"),
		keyVolume:    entityKey("volume"),
		keyCPU:       entityKey("cpu_temperature"),
		keyMemory:    entityKey("memory_available"),
		keyJack:      entityKey("jack_volume"),
		keyJackOn:    entityKey("audio_jack"),
		keySound:     entityKey("speaker_playing"),
		keySpeaker:   entityKey("speaker"),
		keyMicMute:   entityKey("microphone_muted"),
		keyADB:       entityKey("network_adb"),
		keyAlexa:     entityKey("alexa_registered"),
		keyText:      entityKey("alexa_command"),
		keyCommand:   entityKey("send_command"),
		buttons:      newButtons(),
		uptime:       device.UptimeSeconds,
		wifi:         device.WifiSignal,
		volumes:      device.MusicVolumes,
		jack:         device.JackOccupied,
		sound:        device.SpeakerPlaying,
		cpu:          device.CPUTemperature,
		micMute:      device.MicMuted,
		alexa:        device.AlexaRegistered,
		micPress: func() error {
			return fmt.Errorf("no button to press: the switch is not wired to one")
		},
		adbMode:       device.CurrentADBMode,
		adbSet:        device.SetADBMode,
		adbSecureOK:   device.ADBSecureAvailable,
		adbHold:       device.HoldADBOpen,
		adbDeny:       device.DenyADB,
		memory:        device.AvailableMemory,
		handshakeWait: 10 * time.Second,
		pingWait:      pingAfter,
		wakeGap:       minLiveReadGap,
		adbSettle:     adbSettleFor,
		micSettle:     micSettleFor,
		volumeSettle:  volumeSettleFor,
		onDelay:       SoundOnDelay,
		offDelay:      SoundOffDelay,
		conns:         map[*conn]struct{}{},
		published:     map[uint32]reading{},
		liveWake:      make(chan struct{}, 1),
		sensorWake:    make(chan struct{}, 1),
	}
}

func newButtons() []*physicalButton {
	var out []*physicalButton
	for _, b := range []struct{ objectID, name string }{
		{"action_button", "Action button"},
		{"mute_button", "Mute button"},
	} {
		out = append(out, &physicalButton{
			objectID: b.objectID,
			name:     b.name,
			keyEvent: entityKey(b.objectID),
			keyMode:  entityKey(b.objectID + "_mode"),
			mode:     shipped(buttonModes[0]),
		})
	}
	return out
}

func (s *Server) Buttons() []string {
	out := make([]string, 0, len(s.buttons))
	for _, b := range s.buttons {
		out = append(out, b.objectID)
	}
	return out
}

func (s *Server) HasButton(objectID string) bool { return s.button(objectID) != nil }

func (s *Server) button(objectID string) *physicalButton {
	for _, b := range s.buttons {
		if b.objectID == objectID {
			return b
		}
	}
	return nil
}

func (s *Server) UseButton(objectID string, mode func() string, setMode func(string)) {
	if b := s.button(objectID); b != nil {
		b.mode, b.setMode = mode, setMode
	}
}

func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Printf("esphome api listening on %s (device name %q, mac %s)", addr, s.name, s.mac)
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			s.untrustedLog.Printf("esphome api: accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(netConn net.Conn) {
	conn := &conn{sock: netConn, out: make(chan frame, sendQueue)}

	s.mu.Lock()
	if len(s.conns) >= maxConns {
		s.mu.Unlock()
		s.untrustedLog.Printf("esphome api: %s refused: %d connections already", netConn.RemoteAddr(), maxConns)
		netConn.Close()
		return
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()

	s.untrustedLog.Printf("esphome api: %s connected", netConn.RemoteAddr())
	written := make(chan struct{})
	writing := false
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		if writing {
			close(conn.out)
			select {
			case <-written:
			case <-time.After(2 * time.Second):
			}
		}
		netConn.Close()
		s.untrustedLog.Printf("esphome api: %s disconnected", netConn.RemoteAddr())
	}()

	reader := bufio.NewReader(netConn)
	writer := bufio.NewWriter(netConn)

	handshakeDeadline := time.Now().Add(s.handshakeWait)
	netConn.SetReadDeadline(handshakeDeadline)
	lead, err := reader.Peek(1)
	if err != nil {
		s.untrustedLog.Printf("esphome api: %s: %v", netConn.RemoteAddr(), err)
		return
	}
	if lead[0] != leadEncrypted {
		s.untrustedLog.Printf("esphome api: %s tried plaintext", netConn.RemoteAddr())
		netConn.SetWriteDeadline(time.Now().Add(s.handshakeWait))
		_ = writeNoiseFrame(writer, nil)
		_ = writer.Flush()
		return
	}

	session, err := noiseAccept(netConn, reader, writer, s.name, s.psk, handshakeDeadline)
	if err != nil {
		s.untrustedLog.Printf("esphome api: %s handshake failed: %v", netConn.RemoteAddr(), err)
		return
	}
	conn.rw = session
	s.untrustedLog.Printf("esphome api: %s encrypted session established", netConn.RemoteAddr())

	writing = true
	go func() {
		s.writeLoop(conn)
		close(written)
	}()

	decrypted := false
	pinged := false
	for {
		if decrypted {
			netConn.SetReadDeadline(time.Now().Add(s.readWait(pinged)))
		} else {
			netConn.SetReadDeadline(handshakeDeadline)
		}
		msgType, payload, err := conn.rw.read()
		if err != nil {
			if decrypted && !pinged && resumable(err) {
				pinged = true
				if err := s.send(conn, msgPingRequest, nil); err != nil {
					s.untrustedLog.Printf("esphome api: %s ping: %v", netConn.RemoteAddr(), err)
					return
				}
				continue
			}
			s.untrustedLog.Printf("esphome api: %s read: %v", netConn.RemoteAddr(), err)
			return
		}
		decrypted = true
		pinged = false
		if err := s.handle(conn, msgType, payload); err != nil {
			s.untrustedLog.Printf("esphome api: %s handling message %d: %v", netConn.RemoteAddr(), msgType, err)
			return
		}
		s.noteFrom(conn)
	}
}

func (s *Server) noteFrom(conn *conn) {
	if conn.said != "" {
		s.untrustedLog.Printf("esphome api: %s hello from %q", conn.sock.RemoteAddr(), conn.said)
		conn.said = ""
	}
	if conn.noted != "" {
		s.untrustedLog.Printf("%s", conn.noted)
		conn.noted = ""
	}
}

func (s *Server) send(conn *conn, msgType int, payload []byte) error {
	select {
	case conn.out <- frame{msgType, payload}:
		return nil
	default:
		return fmt.Errorf("send queue full after %d frames", cap(conn.out))
	}
}

func (s *Server) writeLoop(conn *conn) {
	for f := range conn.out {
		if err := conn.rw.write(f.msgType, f.payload); err != nil {
			s.untrustedLog.Printf("esphome api: %s write: %v", conn.sock.RemoteAddr(), err)
			conn.sock.Close()
			return
		}
	}
}

func walk(what string, payload []byte, fn func(pbField)) error {
	if err := pbWalk(payload, fn); err != nil {
		return fmt.Errorf("malformed %s: %w", what, err)
	}
	return nil
}

func resumable(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, errMidFrame)
}

func (s *Server) readWait(pinged bool) time.Duration {
	if pinged {
		return s.pingWait * 3 / 2
	}
	return s.pingWait
}

func (s *Server) handle(conn *conn, msgType int, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch msgType {
	case msgHelloRequest:
		var client string
		if err := walk("HelloRequest", payload, func(f pbField) {
			if f.field == 1 {
				client = string(f.data)
			}
		}); err != nil {
			return err
		}
		conn.said = untrustedlog.Cut(client)
		var msg pb
		msg.u32(1, 1)  // api_version_major
		msg.u32(2, 12) // api_version_minor
		msg.str(3, "overdub")
		msg.str(4, s.name)
		return s.send(conn, msgHelloResponse, msg.b)

	case msgConnectRequest:
		var msg pb
		msg.boolean(1, false) // invalid_password
		return s.send(conn, msgConnectResponse, msg.b)

	case msgPingRequest:
		return s.send(conn, msgPingResponse, nil)

	case msgDeviceInfoRequest:
		return s.send(conn, msgDeviceInfoResp, s.deviceInfo())

	case msgListEntitiesReq:
		return s.listEntities(conn)

	case msgSubscribeStates:
		first := !conn.states
		conn.states = true
		if err := s.sendSensorsAt(conn, s.snapshot()); err != nil {
			return err
		}
		if first && !s.stateSubscriberBesides(conn) {
			for _, wake := range []chan struct{}{s.liveWake, s.sensorWake} {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
		return nil

	case msgTextCommand:
		var key uint32
		var text string
		if err := walk("TextCommandRequest", payload, func(f pbField) {
			switch f.field {
			case 1:
				key = uint32(f.num)
			case 2:
				text = string(f.data)
			}
		}); err != nil {
			return err
		}
		if key == s.keyText {
			s.commandLocked(conn, text)
		}
		return nil

	case msgExecuteService:
		var key uint32
		var text string
		var argErr error
		if err := walk("ExecuteServiceRequest", payload, func(f pbField) {
			switch f.field {
			case 1:
				key = uint32(f.num)
			case 2:
				if err := pbWalk(f.data, func(a pbField) {
					if a.field == 4 && text == "" {
						text = string(a.data)
					}
				}); err != nil && argErr == nil {
					argErr = err
				}
			}
		}); err != nil {
			return err
		}
		if argErr != nil {
			return fmt.Errorf("ExecuteServiceArgument: %w", argErr)
		}
		if key == s.keyCommand {
			s.commandLocked(conn, text)
		}
		return nil

	case msgDisconnectRequest:
		s.send(conn, msgDisconnectResp, nil)
		return fmt.Errorf("home assistant asked to disconnect")

	case msgSelectCommand:
		var key uint32
		var choice string
		if err := walk("SelectCommandRequest", payload, func(f pbField) {
			switch f.field {
			case 1:
				key = uint32(f.num)
			case 2:
				choice = string(f.data)
			}
		}); err != nil {
			return err
		}
		if key == s.keyADB {
			s.setADBLocked(conn, choice)
			return nil
		}
		for _, b := range s.buttons {
			if key == b.keyMode {
				s.setModeLocked(conn, b, choice)
				break
			}
		}
		return nil

	case msgSwitchCommand:
		var key uint32
		var on bool
		if err := walk("SwitchCommandRequest", payload, func(f pbField) {
			switch f.field {
			case 1:
				key = uint32(f.num)
			case 2:
				on = f.num != 0
			}
		}); err != nil {
			return err
		}
		if key == s.keyMicMute {
			s.setMicLocked(conn, on)
		}
		return nil

	case msgMediaPlayerCmd:
		var key uint32
		var command uint64
		var hasCommand, hasVolume, volumeIsFloat, hasURL, announcement bool
		var volume float32
		var url string
		if err := walk("MediaPlayerCommandRequest", payload, func(f pbField) {
			switch f.field {
			case 1:
				key = uint32(f.num)
			case 2:
				hasCommand = f.num != 0
			case 3:
				command = f.num
			case 4:
				hasVolume = f.num != 0
			case 5:
				if f.wire == wireFixed32 {
					volume, volumeIsFloat = math.Float32frombits(uint32(f.num)), true
				}
			case 6:
				hasURL = f.num != 0
			case 7:
				url = string(f.data)
			case 9:
				announcement = f.num != 0
			}
		}); err != nil {
			return err
		}
		if key != s.keySpeaker {
			return nil
		}
		switch {
		case hasURL && url != "":
			s.playLocked(conn, url, announcement)
		case hasVolume && volumeIsFloat && isFinite(volume):
			s.setVolumeLocked(conn, volumeWant{fraction: volume, absolute: true})
		case hasCommand && command == mediaVolumeUp:
			s.setVolumeLocked(conn, volumeWant{steps: 1})
		case hasCommand && command == mediaVolumeDown:
			s.setVolumeLocked(conn, volumeWant{steps: -1})
		}
		return nil

	case msgSubscribeHAServ:
		conn.services = true
		return nil

	case msgSubscribeLogs, msgSubscribeHAStates:
		return nil

	default:
		return nil
	}
}

func (s *Server) setModeLocked(conn *conn, b *physicalButton, choice string) {
	if !slices.Contains(buttonModes, choice) {
		conn.noted = fmt.Sprintf("esphome api: %s asked %s for mode %q, which was not offered",
			conn.sock.RemoteAddr(), b.objectID, untrustedlog.Cut(choice))
		return
	}
	if b.setMode == nil || b.mode() == choice {
		return
	}
	b.setMode(choice)
	conn.noted = fmt.Sprintf("esphome api: %s set %s to %s",
		conn.sock.RemoteAddr(), b.objectID, choice)
	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
}

func (s *Server) setADBLocked(conn *conn, choice string) {
	want, ok := device.ParseADBMode(choice)
	if !ok || (want == device.ADBSecure && !s.adbSecureOK()) {
		conn.noted = fmt.Sprintf("esphome api: %s asked network adb for %q, which was not offered",
			conn.sock.RemoteAddr(), untrustedlog.Cut(choice))
		return
	}
	if s.adbHasPending || s.adbWorking {
		if s.adbPending == want {
			return
		}
	} else if s.adbSettled && s.adbLive == want {
		return
	}
	conn.noted = fmt.Sprintf("esphome api: %s set network adb to %s", conn.sock.RemoteAddr(), want)
	s.adbPending, s.adbHasPending = want, true
	if s.adbWorking {
		return
	}
	s.adbWorking = true
	go s.adbWorker()
}

func (s *Server) adbWorker() {
	for {
		s.mu.Lock()
		if !s.adbHasPending {
			s.adbWorking = false
			s.mu.Unlock()
			return
		}
		want := s.adbPending
		s.adbHasPending = false
		s.mu.Unlock()

		s.setADBMode(want)
	}
}

const adbSettleFor = 2 * time.Second

func (s *Server) setADBMode(want device.ADBMode) {
	err := s.adbSet(want)
	if err != nil {
		s.untrustedLog.Printf("network adb: %v", err)
	}
	time.Sleep(s.adbSettle)

	live, known := s.adbMode()
	switch {
	case !known:
		s.untrustedLog.Printf("network adb: asked for %v, and the device could not be read", want)
	case live != want:
		s.untrustedLog.Printf("network adb: asked for %v, device is %v", want, live)
		if want == device.ADBSecure && live == device.ADBInsecure {
			if err := s.adbSet(device.ADBOff); err != nil {
				s.untrustedLog.Printf("network adb: %v", err)
			}
			time.Sleep(s.adbSettle)
			live, known = s.adbMode()
			if known {
				s.untrustedLog.Printf("network adb: closed instead; device is %v", live)
			} else {
				s.untrustedLog.Printf("network adb: closed instead, and the device could not be read")
			}
		}
	case live == device.ADBSecure && err == nil:
		s.untrustedLog.Printf("network adb: LISTENING on tcp/%d, key required; root is still one su away", device.ADBPort)
	case live == device.ADBInsecure:
		s.untrustedLog.Printf("network adb: LISTENING on tcp/%d with no authentication; this is a root-capable shell", device.ADBPort)
	}

	if known && live == device.ADBOff {
		if err := s.adbDeny(); err != nil {
			s.untrustedLog.Printf("network adb: %v", err)
		}
	}

	s.mu.Lock()
	s.adbSettled = known && err == nil && live == want
	if s.adbSettled {
		s.adbLive = live
	}
	s.mu.Unlock()

	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
}

func (s *Server) adbObserved(mode device.ADBMode) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.adbSettled && s.adbLive != mode {
		s.adbSettled = false
	}
}

func (s *Server) UseMicMute(press func() error) {
	s.micPress = press
}

func micWord(muted bool) string {
	if muted {
		return "muted"
	}
	return "live"
}

func (s *Server) setMicLocked(conn *conn, want bool) {
	if s.micHasPending || s.micWorking {
		if s.micPending == want {
			return
		}
	} else if s.micSettled && s.micLive == want {
		return
	}
	conn.noted = fmt.Sprintf("esphome api: %s set the microphone to %s",
		conn.sock.RemoteAddr(), micWord(want))
	s.micPending, s.micHasPending = want, true
	if s.micWorking {
		return
	}
	s.micWorking = true
	go s.micWorker()
}

func (s *Server) micWorker() {
	for {
		s.mu.Lock()
		if !s.micHasPending {
			s.micWorking = false
			s.mu.Unlock()
			return
		}
		want := s.micPending
		s.micHasPending = false
		s.mu.Unlock()

		s.setMic(want)
	}
}

const micSettleFor = 250 * time.Millisecond

func (s *Server) setMic(want bool) {
	muted, settled := s.applyMic(want)

	s.mu.Lock()
	s.micSettled = settled
	if settled {
		s.micLive = muted
	}
	s.mu.Unlock()

	select {
	case s.liveWake <- struct{}{}:
	default:
	}
}

func (s *Server) applyMic(want bool) (muted, settled bool) {
	muted, known := s.micMute()
	if !known {
		s.untrustedLog.Printf("microphone: asked for %s, and the device could not be read", micWord(want))
		return false, false
	}
	if muted == want {
		return muted, true
	}
	if err := s.micPress(); err != nil {
		s.untrustedLog.Printf("microphone: %v", err)
		return false, false
	}
	time.Sleep(s.micSettle)

	muted, known = s.micMute()
	switch {
	case !known:
		s.untrustedLog.Printf("microphone: asked for %s, and the device could not be read back", micWord(want))
		return false, false
	case muted != want:
		s.untrustedLog.Printf("microphone: asked for %s, device is %s", micWord(want), micWord(muted))
		return muted, false
	}
	s.untrustedLog.Printf("microphone: %s", micWord(muted))
	return muted, true
}

func (s *Server) micObserved(muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.micSettled && s.micLive != muted {
		s.micSettled = false
	}
}

func (s *Server) adbBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adbWorking || s.adbHasPending
}
