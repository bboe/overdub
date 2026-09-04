// Package esphome pretends to be an ESPHome device, so Home Assistant adopts
// the Echo Dot with its own first-party integration: no custom component and no
// MQTT. The API is encrypted, and the pre-shared key it needs is the one
// credential the Dot holds.
// docs/architecture.md has the measurements.
package esphome

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bboe/overdub/internal/device"
)

const (
	maxConns  = 8
	sendQueue = 64

	// ESPHome's own numbers: the device pings after this much silence and gives
	// up at two and a half times it. docs/architecture.md says why the ratio is
	// worth copying.
	pingAfter = 60 * time.Second
	idleWait  = pingAfter * 5 / 2

	// clientKeepalive is aioesphomeapi's KEEP_ALIVE_FREQUENCY.
	clientKeepalive = 20 * time.Second

	// The shortest gap between two reads a wake can ask for, on either poll,
	// and the floor under a PollLive tick that is not positive. The volume's
	// fork is what the length of it is for; what it bounds is a peer, since
	// both wakes are things a peer can ask for as fast as it can send.
	minLiveReadGap = time.Second

	// How many of PollLive's ticks pass between one reading of the expensive
	// sensors and the next. The tick is what the poll wakes on; this is what it
	// reads on, and they are separate numbers so that a reading cheap enough to
	// want oftener than a fork can have a divisor of its own rather than
	// dragging the fork along with it. Five half-seconds is the two and a half
	// second interval this poll has always read on.
	HeavyEvery = 5

	// The sound reading's own divisor against the same tick.
	SoundEvery = 1

	// MinSensorTick is the floor under PollSensors' ticker; PollLive has none.
	// It bounds that ticker rather than the push rate, which a subscriber's wake
	// can exceed, and rather than the connection's life, which the ping keeps.
	// docs/architecture.md has the measurement it came from.
	MinSensorTick = 30 * time.Second

	// The log is a file on /data that is truncated at boot, and every line below
	// is written because an unauthenticated peer did something. Rate-limited, and
	// capped for the run as well: nothing truncates the log while the daemon
	// lives, so a rate alone still grows without bound on a Dot whose uptime is
	// measured in months.
	logWindow = time.Minute
	logBurst  = 20
	logTotal  = 5000

	// What a peer sends is quoted into that log, so it is cut to a length worth
	// reading. A full frame of \xff quotes to four times its size on one line.
	maxLoggedString = 64
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
	msgListEntitiesDone  = 19
	msgSubscribeStates   = 20
	msgBinarySensorState = 21
	msgSensorState       = 25
	msgSubscribeLogs     = 28
	msgSubscribeHAServ   = 34
	msgHomeassistantAct  = 35
	msgSubscribeHAStates = 38
	msgListSelect        = 52
	msgSelectState       = 53
	msgSelectCommand     = 54
	msgListEvent         = 107
	msgEventState        = 108
)

// A physical button on the Dot: an event entity reporting what it did, and a
// select saying what the daemon does with it. Every button here has both, so
// there is no button that reports without being configurable or the reverse.
type physicalButton struct {
	objectID string // the event entity's, and the select's with "_mode" on it
	name     string
	keyEvent uint32
	keyMode  uint32

	// What the button is doing, and the ask to change it. The button owns its
	// mode, so there is one copy of it rather than two that could disagree: the
	// read loop consults its own field on every event and cannot take the
	// server lock to do it. setMode is called with the server lock held and
	// mode is called from readTicked without it, so the rule for both is the
	// stricter one: neither may block, whichever holds what. Set together by
	// UseButton and never apart, because either alone is a select that lies.
	// setMode is nil on a server built without that button, which is a test
	// rather than a state a Dot can be in, and the select then reports the
	// shipped mode and moves nothing.
	mode    func() string
	setMode func(string)
}

// What a button reports before anything wires it, so a server with none reports
// a mode rather than an empty string.
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

	// Sent SubscribeHomeassistantServicesRequest. A separate subscription from
	// states, and the only thing that reads it is the press count: docs/
	// architecture.md says why a count cannot ride the event itself.
	services bool

	// Written by handle and read by serveConn, both on this connection's own
	// goroutine, so that the log write happens with no lock held.
	said string

	// A whole line handle wants in the log, carried out for the same reason.
	noted string
}

type Server struct {
	name  string
	model string
	mac   string

	psk []byte

	keyUptime uint32
	keyWifi   uint32
	keyVolume uint32
	keyCPU    uint32
	keyMemory uint32
	keyJack   uint32
	keyJackOn uint32
	keySound  uint32
	keyADB    uint32

	// The physical buttons, in the order they are listed. Each is an event
	// entity, which reports a moment rather than a value and so is never
	// published, and a select, which is the only kind of entity a peer writes.
	// A slice rather than a map, because the order is what Home Assistant shows.
	buttons []*physicalButton

	// One worker at a time, and one mode waiting for it. Home Assistant can
	// move a dropdown faster than adbd restarts, and every position asked for
	// in between is a position nobody wants to arrive at: collapsing them to
	// the latest is what makes the select settle where it was last put.
	adbWorking    bool
	adbHasPending bool
	adbPending    device.ADBMode

	// Whether the last apply ended where it was aimed. The no-op guard below
	// rests on it: without it a position that half-took is reported as reached
	// and can never be asked for again, so an operator watching a stale rule
	// has no way to drive it out from the dropdown.
	adbSettled bool

	// PollLive's alone, and unlocked because of it.
	soundOn  bool
	soundGap time.Duration
	// The delays, per server rather than per package, so a test that shrinks
	// them cannot reach another test's poll.
	onDelay     time.Duration
	offDelay    time.Duration
	soundSince  time.Time
	soundLastOn time.Time
	soundSeen   time.Time

	// Read the device, so the tests can answer for them: /proc/uptime and
	// /proc/net/wireless are Linux's, and the tests run wherever the developer is.
	// The adb three are here for a second reason as well: the setter restarts
	// adbd and opens a port, which no test may do.
	uptime  func() (float32, bool)
	wifi    func() (float32, bool)
	volumes func() device.MusicVolume
	jack    func() (bool, bool)
	sound   func() (bool, bool)
	cpu     func() (float32, bool)
	memory  func() (float32, bool)

	adbMode     func() (device.ADBMode, bool)
	adbSet      func(device.ADBMode) error
	adbSecureOK func() bool
	adbHold     func() error
	adbDeny     func() error

	// Fields so a test can shrink them without racing another test's server.
	handshakeWait time.Duration
	pingWait      time.Duration
	wakeGap       time.Duration
	adbSettle     time.Duration

	mu    sync.Mutex
	conns map[*conn]struct{}

	// What every subscriber has been told, and the only thing any of them is
	// ever told. docs/architecture.md says why a second reader of the device
	// would put a connection out of step with this.
	published map[uint32]reading

	// Woken when a connection subscribes for the first time, so the value it was
	// answered with is corrected by a read rather than by the next tick.
	// Buffered by one and never blocked on: a wake already waiting is a read
	// already coming.
	liveWake   chan struct{}
	sensorWake chan struct{}

	logMu        sync.Mutex
	logWindowEnd time.Time
	logLines     int
	logDropped   int
	logWritten   int
}

func NewServer(name, model, mac string, psk []byte) *Server {
	return &Server{
		name:          name,
		model:         model,
		mac:           mac,
		psk:           psk,
		keyUptime:     entityKey("uptime"),
		keyWifi:       entityKey("wifi_signal"),
		keyVolume:     entityKey("volume"),
		keyCPU:        entityKey("cpu_temperature"),
		keyMemory:     entityKey("memory_available"),
		keyJack:       entityKey("jack_volume"),
		keyJackOn:     entityKey("audio_jack"),
		keySound:      entityKey("speaker_playing"),
		keyADB:        entityKey("network_adb"),
		buttons:       newButtons(),
		uptime:        device.UptimeSeconds,
		wifi:          device.WifiSignal,
		volumes:       device.MusicVolumes,
		jack:          device.JackOccupied,
		sound:         device.SpeakerPlaying,
		cpu:           device.CPUTemperature,
		adbMode:       device.CurrentADBMode,
		adbSet:        device.SetADBMode,
		adbSecureOK:   device.ADBSecureAvailable,
		adbHold:       device.HoldADBOpen,
		adbDeny:       func() error { return device.DenyTCP(device.ADBPort) },
		memory:        device.AvailableMemory,
		handshakeWait: 10 * time.Second,
		pingWait:      pingAfter,
		wakeGap:       minLiveReadGap,
		adbSettle:     adbSettleFor,
		onDelay:       SoundOnDelay,
		offDelay:      SoundOffDelay,
		conns:         map[*conn]struct{}{},
		published:     map[uint32]reading{},
		liveWake:      make(chan struct{}, 1),
		sensorWake:    make(chan struct{}, 1),
	}
}

// The buttons a Dot has, in listing order. Named here rather than by the caller
// because the object ids are what Home Assistant files entities under, and a
// name invented per call site is one that can differ between two of them.
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
			// A placeholder, not this button's shipped mode: the caller owns
			// the keys and therefore what each starts in, and UseButton
			// replaces this before any connection exists. Naming the real mode
			// here would state it twice with nothing holding the two together.
			mode: shipped(buttonModes[0]),
		})
	}
	return out
}

// Buttons names every button this server reports, in listing order. The caller
// wires the keys, so this is what lets it check that it has one for each.
func (s *Server) Buttons() []string {
	out := make([]string, 0, len(s.buttons))
	for _, b := range s.buttons {
		out = append(out, b.objectID)
	}
	return out
}

// HasButton says whether an object id names a button this server reports. The
// caller wires the keys, and a name it has that this does not is one whose
// presses reach nobody.
func (s *Server) HasButton(objectID string) bool { return s.button(objectID) != nil }

// The button an object id names, or nil.
func (s *Server) button(objectID string) *physicalButton {
	for _, b := range s.buttons {
		if b.objectID == objectID {
			return b
		}
	}
	return nil
}

// UseButton wires the select to the button, reader and writer in one call.
// Two exported fields let a caller set the writer and leave the reader at the
// default, which is a select that really moves the button and reports the
// shipped mode for ever: Home Assistant's dropdown snaps back on every poll and
// nothing says why.
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
			s.peerLogf("esphome api: accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go s.serveConn(c)
	}
}

// peerLogf logs what a peer caused, within the rate limit. The format string
// reaches log.Printf unmodified, so go vet still checks the verbs here.
func (s *Server) peerLogf(format string, args ...any) {
	dropped, allow, last := s.logAllow()
	if dropped > 0 {
		log.Printf("esphome api: %d lines suppressed", dropped)
	}
	if allow {
		log.Printf(format, args...)
	}
	if last {
		log.Printf("esphome api: %d lines this run; nothing a peer does is logged"+
			" again until a restart", logTotal)
	}
}

func (s *Server) logAllow() (dropped int, allow, last bool) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	// Silent for the rest of the run, the count of what was dropped included:
	// one line a minute saying so is itself unbounded growth.
	if s.logWritten >= logTotal {
		return 0, false, false
	}
	if now := time.Now(); now.After(s.logWindowEnd) {
		s.logWindowEnd = now.Add(logWindow)
		s.logLines = 0
		dropped, s.logDropped = s.logDropped, 0
	}
	if s.logLines >= logBurst {
		s.logDropped++
		return dropped, false, false
	}
	s.logLines++
	s.logWritten++
	return dropped, true, s.logWritten == logTotal
}

func truncate(s string) string {
	if len(s) > maxLoggedString {
		return s[:maxLoggedString] + "..."
	}
	return s
}

func (s *Server) serveConn(netConn net.Conn) {
	conn := &conn{sock: netConn, out: make(chan frame, sendQueue)}

	s.mu.Lock()
	if len(s.conns) >= maxConns {
		s.mu.Unlock()
		s.peerLogf("esphome api: %s refused: %d connections already", netConn.RemoteAddr(), maxConns)
		netConn.Close()
		return
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()

	s.peerLogf("esphome api: %s connected", netConn.RemoteAddr())
	written := make(chan struct{})
	writing := false
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		// Let what is already queued reach the wire. The reply to a
		// DisconnectRequest is queued and then this returns, and closing the socket
		// first turns an orderly goodbye into an EOF at the other end. Only if the
		// writer was ever started: a connection refused at the handshake has no
		// goroutine to wait for.
		if writing {
			close(conn.out)
			select {
			case <-written:
			case <-time.After(2 * time.Second):
			}
		}
		netConn.Close()
		s.peerLogf("esphome api: %s disconnected", netConn.RemoteAddr())
	}()

	reader := bufio.NewReader(netConn)
	writer := bufio.NewWriter(netConn)

	handshakeDeadline := time.Now().Add(s.handshakeWait)
	netConn.SetReadDeadline(handshakeDeadline)
	lead, err := reader.Peek(1)
	if err != nil {
		s.peerLogf("esphome api: %s: %v", netConn.RemoteAddr(), err)
		return
	}
	if lead[0] != leadEncrypted {
		s.peerLogf("esphome api: %s tried plaintext", netConn.RemoteAddr())
		// A budget of its own, not what is left of the handshake's.
		netConn.SetWriteDeadline(time.Now().Add(s.handshakeWait))
		_ = writeNoiseFrame(writer, nil)
		_ = writer.Flush()
		return
	}

	session, err := noiseAccept(netConn, reader, writer, s.name, s.psk, handshakeDeadline)
	if err != nil {
		s.peerLogf("esphome api: %s handshake failed: %v", netConn.RemoteAddr(), err)
		return
	}
	conn.rw = session
	s.peerLogf("esphome api: %s encrypted session established", netConn.RemoteAddr())

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
			// aioesphomeapi pings only when it has not heard from the device, so
			// a device that talks enough is never pinged and a deadline waiting
			// for that ping expires on a connection that is working. Asking is
			// what makes the deadline ours: the reply is a read, and a peer that
			// cannot answer one is gone whatever it was sending.
			if decrypted && !pinged && resumable(err) {
				pinged = true
				if err := s.send(conn, msgPingRequest, nil); err != nil {
					s.peerLogf("esphome api: %s ping: %v", netConn.RemoteAddr(), err)
					return
				}
				continue
			}
			s.peerLogf("esphome api: %s read: %v", netConn.RemoteAddr(), err)
			return
		}
		decrypted = true
		pinged = false
		if err := s.handle(conn, msgType, payload); err != nil {
			s.peerLogf("esphome api: %s handling message %d: %v", netConn.RemoteAddr(), msgType, err)
			return
		}
		if conn.said != "" {
			s.peerLogf("esphome api: %s hello from %q", netConn.RemoteAddr(), conn.said)
			conn.said = ""
		}
		if conn.noted != "" {
			s.peerLogf("%s", conn.noted)
			conn.noted = ""
		}
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
			s.peerLogf("esphome api: %s write: %v", conn.sock.RemoteAddr(), err)
			conn.sock.Close()
			return
		}
	}
}

// Reported by its error rather than logged where it is found, so that nothing
// writes to the log with the server lock held.
func walk(what string, payload []byte, fn func(pbField)) error {
	if err := pbWalk(payload, fn); err != nil {
		return fmt.Errorf("malformed %s: %w", what, err)
	}
	return nil
}

// Whether the ping can take over a read that has just expired. Only a read that
// took nothing off the socket qualifies: docs/architecture.md says why a frame
// that stopped part-way cannot be retried.
func resumable(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, errMidFrame)
}

// Silence before the ping, and what is left of the budget after it. The two
// together are idleWait. docs/architecture.md says why the second is the longer.
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
		// Answering a message we could not parse means replying to whatever was
		// scraped out of it before it went wrong.
		if err := walk("HelloRequest", payload, func(f pbField) {
			if f.field == 1 {
				client = string(f.data)
			}
		}); err != nil {
			return err
		}
		conn.said = truncate(client)
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
		// Only the first one wakes the poll. ESPHome's client subscribes once,
		// and a peer that asks again would otherwise be asking for a reading of
		// the device, as fast as it can send the request.
		first := !conn.states
		conn.states = true
		// The snapshot first, then the polls are woken: a value not read for a
		// while is corrected a read later rather than withheld.
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

	case msgDisconnectRequest:
		s.send(conn, msgDisconnectResp, nil)
		return fmt.Errorf("home assistant asked to disconnect")

	case msgSelectCommand:
		var key uint32
		var choice string
		// The second message whose payload is read, and read for the same
		// reason the first is: acting on one that did not parse means acting on
		// whatever pbWalk scraped out before it stopped.
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

	case msgSubscribeHAServ:
		conn.services = true
		return nil

	case msgSubscribeLogs, msgSubscribeHAStates:
		return nil

	default:
		return nil
	}
}

// The button is told and the poll is woken; nothing is published from here,
// because publish takes the server lock and this runs under it. Caller holds mu.
func (s *Server) setModeLocked(conn *conn, b *physicalButton, choice string) {
	// A mode we never offered is not one to act on: the listing is what Home
	// Assistant was told it could pick from, so anything else is a peer making
	// something up rather than a user choosing.
	if !slices.Contains(buttonModes, choice) {
		conn.noted = fmt.Sprintf("esphome api: %s asked %s for mode %q, which was not offered",
			conn.sock.RemoteAddr(), b.objectID, truncate(choice))
		return
	}
	// No button to move, or it is already where it is being asked to go. This
	// turns away a repeat and nothing else: a peer cycling the modes changes
	// the state every time and so passes every time. What bounds that is the
	// wake gap on the poll below, not this.
	if b.setMode == nil || b.mode() == choice {
		return
	}
	b.setMode(choice)
	// Which way the button went is the most consequential thing a peer can ask
	// for here, and the only record separating a button nobody is answering
	// from one Home Assistant let go of. Carried out of the lock like the hello
	// line, because the log is a file on /data.
	conn.noted = fmt.Sprintf("esphome api: %s set %s to %s",
		conn.sock.RemoteAddr(), b.objectID, choice)
	// The state Home Assistant is waiting for goes out on the next turn of the
	// sensor poll, which is what publishes this reading at all. That turn is
	// gated by the wake gap, so a peer toggling as fast as it can send does not
	// drive a procfs read and a push per message.
	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
}

// A mode the listing did not offer is refused rather than acted on, the way a
// button mode is, and Secure is refused for the same reason when there is no
// key to authenticate against: adbd would come up open instead, which is the
// one position nobody asked for.
//
// Caller holds mu, so this may not do the work. SetADBMode restarts adbd and
// then waits to see what the device settled on, which is seconds spent under a
// lock that gates the accept path and every other connection. It hands the mode
// to a worker instead, and nothing is published from here.
func (s *Server) setADBLocked(conn *conn, choice string) {
	want, ok := device.ParseADBMode(choice)
	if !ok || (want == device.ADBSecure && !s.adbSecureOK()) {
		conn.noted = fmt.Sprintf("esphome api: %s asked network adb for %q, which was not offered",
			conn.sock.RemoteAddr(), truncate(choice))
		return
	}
	// Already there, or already on its way there. The button select turns away
	// a repeat to save a wake; here it saves an adbd restart, which drops every
	// live adb session -- so a peer resending one position on a connection it
	// already holds would otherwise cost a restart every couple of seconds for
	// as long as it liked, on a path the eight slots do not bound. Compared
	// against the published state rather than the device, because reading the
	// device forks and this runs under the server lock.
	if s.adbHasPending || s.adbWorking {
		if s.adbPending == want {
			return
		}
	} else if was, told := s.published[s.keyADB]; told && s.adbSettled && was.text == want.String() {
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

// One worker however many moves arrive, taking the latest pending mode each
// time round rather than a queue of them: a dropdown dragged through three
// positions restarts adbd once and lands on the third. Every position in
// between is one nobody wanted to arrive at.
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

// How long adbd is given to restart before the device is asked what it settled
// on. A field on the server rather than a package variable, like the other
// three above: the worker outlives the test that started it, so a test
// restoring a shared one races the goroutine still reading it.
const adbSettleFor = 2 * time.Second

// What the device actually did, rather than what it was told to do. adbd is
// restarted by a property, so nothing here gets a result back: the mode is set,
// the device is given a moment, and then it is read.
//
// The gap between the two is why Secure fails closed. Asking for Secure means
// installing a key and setting ro.adb.secure, and if the property did not take
// while adbd came up anyway, the device is open to the whole subnet with no
// authentication -- a strictly weaker position than the one that was asked for,
// arrived at silently. So that one case is closed rather than reported.
func (s *Server) setADBMode(want device.ADBMode) {
	err := s.adbSet(want)
	if err != nil {
		s.peerLogf("network adb: %v", err)
	}
	time.Sleep(s.adbSettle)

	live, known := s.adbMode()
	switch {
	case !known:
		s.peerLogf("network adb: asked for %v, and the device could not be read", want)
	case live != want:
		s.peerLogf("network adb: asked for %v, device is %v", want, live)
		if want == device.ADBSecure && live == device.ADBInsecure {
			if err := s.adbSet(device.ADBOff); err != nil {
				s.peerLogf("network adb: %v", err)
			}
			// Settled like the first attempt rather than read straight away:
			// adbd is restarted by a property and goes down on its own
			// schedule, so an immediate read finds it still listening.
			time.Sleep(s.adbSettle)
			live, known = s.adbMode()
			s.peerLogf("network adb: closed instead; device is %v", live)
		}
	case live == device.ADBSecure && err == nil:
		s.peerLogf("network adb: LISTENING on tcp/%d, key required; root is still one su away", device.ADBPort)
	case live == device.ADBInsecure:
		s.peerLogf("network adb: LISTENING on tcp/%d with no authentication; this is a root-capable shell", device.ADBPort)
	}

	// A port that ended up closed is closed again here. adbd dies on its own
	// schedule after ctl.restart, so the sensor poll can have found it still
	// listening and put the rule back -- and once adbd is gone nothing else
	// would ever take that rule out: the mode reads Off, so no poll re-asserts
	// it, no later command repeats an Off the guard turns away, and
	// uninstall.sh leaves this port alone on purpose.
	if known && live == device.ADBOff {
		if err := s.adbDeny(); err != nil {
			s.peerLogf("network adb: %v", err)
		}
	}

	s.mu.Lock()
	s.adbSettled = known && err == nil && live == want
	s.mu.Unlock()

	// The poll is woken rather than told, so the device keeps one reader. A
	// publish from here would be a second, and docs/architecture.md gives the
	// case: this end and the poll can read the device either side of a change
	// and the later publish is not the later reading, which leaves Home
	// Assistant holding a mode nothing will correct until the minute is up. The
	// wake is the button select's, and the gap on it bounds this too.
	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
}

// Whether a position is on its way to the device, which is the window where
// what the device says about itself is behind what it has been asked for.
func (s *Server) adbBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adbWorking || s.adbHasPending
}
