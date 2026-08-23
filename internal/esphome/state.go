package esphome

import (
	"fmt"
	"log"
	"time"
)

func (s *Server) sendSensorsAt(conn *conn, up float32, upOK bool, signal float32, signalOK bool) error {
	if err := s.send(conn, msgSensorState, floatState(s.keyUptime, up, !upOK)); err != nil {
		return err
	}
	return s.send(conn, msgSensorState, floatState(s.keyWifi, signal, !signalOK))
}

func floatState(key uint32, v float32, missing bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.float(2, v)
	p.boolean(3, missing)
	return p.b
}

// Returns what it could not send to rather than logging it, because the caller
// holds the server lock and the log is a file on /data.
func (s *Server) eachConn(what string, wants func(*conn) bool, send func(*conn) error) []string {
	var failed []string
	for conn := range s.conns {
		if !wants(conn) {
			continue
		}
		if err := send(conn); err != nil {
			failed = append(failed, fmt.Sprintf("esphome api: %s to %s failed: %v",
				what, conn.sock.RemoteAddr(), err))
			delete(s.conns, conn)
			conn.sock.Close()
		}
	}
	return failed
}

func wantsStates(c *conn) bool { return c.states }

func (s *Server) PollSensors(every time.Duration) {
	if every < MinSensorTick {
		log.Printf("esphome api: sensor tick of %v raised to the %v floor", every, MinSensorTick)
		every = MinSensorTick
	}
	for range time.Tick(every) {
		s.pollOnce()
	}
}

func (s *Server) pollOnce() {
	// Read once, and before the lock: it is the same value for every connection,
	// and procfs under the server lock is the rule this file keeps elsewhere.
	up, upOK := s.uptime()
	signal, signalOK := s.wifi()
	s.mu.Lock()
	failed := s.eachConn("sensors", wantsStates, func(c *conn) error {
		return s.sendSensorsAt(c, up, upOK, signal, signalOK)
	})
	s.mu.Unlock()
	for _, line := range failed {
		s.peerLogf("%s", line)
	}
}
