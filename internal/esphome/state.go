package esphome

import (
	"fmt"
	"log"
	"time"
)

type reading struct {
	key   uint32
	value float32
	ok    bool
	// Sent as a binary sensor state rather than a sensor state. A field here
	// rather than a second published map, because one published state is what
	// makes a subscriber and the polls agree.
	binary bool
}

// The minute's readings: an uptime that changes on every read whatever the
// cadence, and a signal that costs 1.8ms to take.
func (s *Server) readTicked() []reading {
	up, upOK := s.uptime()
	signal, signalOK := s.wifi()
	return []reading{
		{key: s.keyUptime, value: up, ok: upOK},
		{key: s.keyWifi, value: signal, ok: signalOK},
	}
}

// The short tick's readings, taken together because they are published
// together. Measured on the Dot: 118us for the temperature, 111us for the
// memory, 113us for the jack, and 11.7ms for the volume, which is the one that
// forks and so is the whole of the tick's cost.
func (s *Server) readLive() []reading {
	cpu, cpuOK := s.cpu()
	memory, memoryOK := s.memory()
	volumes := s.volumes()
	occupied, jackOK := s.jack()
	return []reading{
		{key: s.keyCPU, value: cpu, ok: cpuOK},
		{key: s.keyMemory, value: memory, ok: memoryOK},
		{key: s.keyVolume, value: volumes.Speaker, ok: volumes.SpeakerOK},
		{key: s.keyJack, value: volumes.Jack, ok: volumes.JackOK},
		{key: s.keyJackOn, value: boolValue(occupied), ok: jackOK, binary: true},
	}
}

func boolValue(b bool) float32 {
	if b {
		return 1
	}
	return 0
}

// What a subscriber is told when it arrives: the published state as it stands,
// and never a reading of its own. Caller holds mu.
func (s *Server) snapshot() []reading {
	readings := make([]reading, 0, len(s.published))
	for _, r := range s.published {
		readings = append(readings, r)
	}
	return readings
}

func (s *Server) sendSensorsAt(conn *conn, readings []reading) error {
	for _, r := range readings {
		msgType, payload := msgSensorState, floatState(r.key, r.value, !r.ok)
		if r.binary {
			msgType, payload = msgBinarySensorState, binaryState(r.key, r.value != 0, !r.ok)
		}
		if err := s.send(conn, msgType, payload); err != nil {
			return err
		}
	}
	return nil
}

func binaryState(key uint32, on, missing bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(2, on)
	p.boolean(3, missing)
	return p.b
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

// The one way a reading reaches anybody. It records what it sends and sends
// only what it has not already recorded, so the published state is exactly what
// every subscriber holds. The snapshot sends too, but only what this has
// already published. The log write waits until the lock is dropped, because the
// lock gates the accept path and every other connection's handler.
func (s *Server) publish(what string, readings []reading) []reading {
	s.mu.Lock()
	var changed []reading
	for _, r := range readings {
		// told rather than the zero reading standing for "not published": every
		// key here is a hash that happens never to be zero, so the two are
		// distinguishable without it, but that is not a thing to rest it on.
		if was, told := s.published[r.key]; told && was == r {
			continue
		}
		s.published[r.key] = r
		changed = append(changed, r)
	}
	var failed []string
	if len(changed) > 0 {
		failed = s.eachConn(what, wantsStates, func(c *conn) error {
			return s.sendSensorsAt(c, changed)
		})
	}
	s.mu.Unlock()
	for _, line := range failed {
		s.peerLogf("%s", line)
	}
	return changed
}

// Starts a poll for every sensor listEntities names.
func (s *Server) Poll(sensorTick, liveTick time.Duration) {
	go s.PollSensors(sensorTick)
	go s.PollLive(liveTick)
}

// Published once before the wait, so a subscriber arriving in the first minute
// is not told nothing at all: time.Tick fires after the interval, not at it.
func (s *Server) PollSensors(every time.Duration) {
	if every < MinSensorTick {
		log.Printf("esphome api: sensor tick of %v raised to the %v floor", every, MinSensorTick)
		every = MinSensorTick
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s.publish("sensors", s.readTicked())
		select {
		case <-tick.C:
		case <-s.sensorWake:
		}
	}
}

func (s *Server) PollLive(every time.Duration) {
	// time.NewTicker panics rather than returning an error, and this poll has no
	// floor of its own for a caller to have been stopped by.
	if every <= 0 {
		log.Printf("esphome api: live tick of %v raised to %v", every, minLiveReadGap)
		every = minLiveReadGap
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var last time.Time
	woken := false
	for {
		if s.anyStateSubscriber() && (!woken || time.Since(last) >= s.liveGap) {
			last = time.Now()
			s.publish("live", s.readLive())
		}
		select {
		case <-tick.C:
			woken = false
		case <-s.liveWake:
			woken = true
		}
	}
}

func (s *Server) anyStateSubscriber() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateSubscriberBesides(nil)
}

// Whether anybody but this connection is subscribed. Caller holds mu.
func (s *Server) stateSubscriberBesides(except *conn) bool {
	for conn := range s.conns {
		if conn != except && conn.states {
			return true
		}
	}
	return false
}
