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

	// The shortest gap between two volume reads a wake can ask for, and the
	// floor under a tick that is not positive.
	minVolumeReadGap = time.Second

	// MinSensorTick is the floor under PollSensors' ticker; PollVolume has none.
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
	msgListSensor        = 16
	msgListEntitiesDone  = 19
	msgSubscribeStates   = 20
	msgSensorState       = 25
	msgSubscribeLogs     = 28
	msgSubscribeHAStates = 38
)

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

	// Written by handle and read by serveConn, both on this connection's own
	// goroutine, so that the log write happens with no lock held.
	said string
}

type Server struct {
	name  string
	model string
	mac   string

	psk []byte

	keyUptime uint32
	keyWifi   uint32
	keyVolume uint32

	// Read the device, so the tests can answer for them: /proc/uptime and
	// /proc/net/wireless are Linux's, and the tests run wherever the developer is.
	uptime func() (float32, bool)
	wifi   func() (float32, bool)
	volume func() (float32, bool)

	// Fields so a test can shrink them without racing another test's server.
	handshakeWait time.Duration
	pingWait      time.Duration
	volumeGap     time.Duration

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
	volumeWake chan struct{}
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
		uptime:        device.UptimeSeconds,
		wifi:          device.WifiSignal,
		volume:        device.MusicVolumePercent,
		handshakeWait: 10 * time.Second,
		pingWait:      pingAfter,
		volumeGap:     minVolumeReadGap,
		conns:         map[*conn]struct{}{},
		published:     map[uint32]reading{},
		volumeWake:    make(chan struct{}, 1),
		sensorWake:    make(chan struct{}, 1),
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
			for _, wake := range []chan struct{}{s.volumeWake, s.sensorWake} {
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

	case msgSubscribeLogs, msgSubscribeHAStates:
		return nil

	default:
		return nil
	}
}
