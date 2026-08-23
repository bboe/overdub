// Package esphome pretends to be an ESPHome device, so Home Assistant adopts
// the Echo Dot with its own first-party integration: no custom component, no
// MQTT, no credential on the Dot.
// docs/architecture.md has the measurements.
package esphome

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"sync"
	"time"

	"github.com/bboe/overdub/internal/device"
)

const (
	maxConns  = 8
	sendQueue = 64

	idleWait = 90 * time.Second

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
	writer *bufio.Writer
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

	keyUptime uint32

	// Reads the device, so the test can answer for it: /proc/uptime is Linux's,
	// and the tests run wherever the developer is.
	uptime func() (float32, bool)

	// A client that has said nothing holds one of the eight slots, so it gets far
	// less rope than one that has introduced itself. A field so the test can
	// shrink it without racing another test's server.
	handshakeWait time.Duration

	mu    sync.Mutex
	conns map[*conn]struct{}

	logMu        sync.Mutex
	logWindowEnd time.Time
	logLines     int
	logDropped   int
	logWritten   int
}

func NewServer(name, model, mac string) *Server {
	return &Server{
		name:          name,
		model:         model,
		mac:           mac,
		keyUptime:     entityKey("uptime"),
		uptime:        device.UptimeSeconds,
		handshakeWait: 10 * time.Second,
		conns:         map[*conn]struct{}{},
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
	conn := &conn{
		sock:   netConn,
		writer: bufio.NewWriter(netConn),
		out:    make(chan frame, sendQueue),
	}

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
	go func() {
		s.writeLoop(conn)
		close(written)
	}()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		// Let what is already queued reach the wire. The reply to a
		// DisconnectRequest is queued and then this returns, and closing the socket
		// first turns an orderly goodbye into an EOF at the other end.
		close(conn.out)
		select {
		case <-written:
		case <-time.After(2 * time.Second):
		}
		netConn.Close()
		s.peerLogf("esphome api: %s disconnected", netConn.RemoteAddr())
	}()

	reader := bufio.NewReader(netConn)
	idle := s.handshakeWait
	for {
		netConn.SetReadDeadline(time.Now().Add(idle))
		msgType, payload, err := readFrame(reader)
		if err != nil {
			s.peerLogf("esphome api: %s read: %v", netConn.RemoteAddr(), err)
			return
		}
		// Introducing itself is what buys the longer wait, and only that: any
		// frame at all would let eight peers hold every slot with one ping
		// apiece.
		if msgType == msgHelloRequest {
			idle = idleWait
		}
		if err := s.handle(conn, msgType, payload); err != nil {
			s.peerLogf("esphome api: %s handling message %d: %v", netConn.RemoteAddr(), msgType, err)
			return
		}
		// The first reading goes out here rather than from handle, which holds the
		// server lock for its whole body: reading procfs underneath that lock
		// stalls the accept path and every other connection.
		if msgType == msgSubscribeStates {
			up, ok := s.uptime()
			if err := s.sendSensorsAt(conn, up, ok); err != nil {
				s.peerLogf("esphome api: %s first sensor push: %v", netConn.RemoteAddr(), err)
				return
			}
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
		conn.sock.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := writeFrame(conn.writer, f.msgType, f.payload); err != nil {
			s.peerLogf("esphome api: %s write: %v", conn.sock.RemoteAddr(), err)
			conn.sock.Close()
			return
		}
		if err := conn.writer.Flush(); err != nil {
			s.peerLogf("esphome api: %s flush: %v", conn.sock.RemoteAddr(), err)
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
		conn.states = true
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
