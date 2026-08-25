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

// SoundOnDelay and SoundOffDelay are how long sound has to last before it is
// reported, and how long it has to be gone before that is withdrawn. Exported
// so main_test.go can hold them against the interval they are sampled on.
const (
	SoundOnDelay  = time.Second
	SoundOffDelay = time.Second
)

// Unlike the other readers this one remembers, so only PollLive may call it.
// Called on the first tick that finds nobody subscribed, because the stretch
// that follows has no bound -- Home Assistant can be away for an hour -- so
// nothing measured before it means anything. Nothing wakes the poll on a
// disconnect, so a Home Assistant back inside that tick keeps the reading, which
// is half a second old rather than stale. The sampling gap guard inside readSound is the bounded
// case and deliberately keeps the reading.
//
// The published reading goes with the hysteresis, and that is the half that
// reaches Home Assistant: a subscriber is answered from the published state
// before the poll has read anything, so a value left there is one a returning
// Home Assistant is told before the first fresh reading can correct it, which
// fires anything triggered on the speaker turning on. Dropping it is only safe
// here, with nobody subscribed: published is otherwise exactly what every
// subscriber holds.
func (s *Server) forgetSound() {
	s.soundOn = false
	s.soundSince, s.soundLastOn, s.soundSeen = time.Time{}, time.Time{}, time.Time{}
	s.mu.Lock()
	delete(s.published, s.keySound)
	s.mu.Unlock()
}

func (s *Server) readSound() reading {
	playing, ok := s.sound()
	now := time.Now()
	// A delay is only two readings if the readings were taken when they were
	// meant to be. PollLive is serial, so a heavy tick whose fork runs long
	// pushes the next sample out, and one sample either side of that gap would
	// otherwise decide an edge on its own.
	gapped := s.soundGap > 0 && !s.soundSeen.IsZero() && now.Sub(s.soundSeen) > s.soundGap
	s.soundSeen = now
	if !ok {
		// soundLastOn is the last time sound was seen, and a read that failed
		// is not a sighting of silence. Zeroing it made the off test measure
		// against the zero time, which is past any delay; moving it forward,
		// which is what the gap guard does, held the entity on across the
		// failure and reported it as playing again afterwards.
		s.soundSince = time.Time{}
		return reading{key: s.keySound, binary: true}
	}
	// Only once there is a reading to hang them on. Carrying the withdrawal's
	// clock forward says sound was seen at a moment nothing was looking, so it
	// is only done while that claim is smaller than the withdrawal itself. A
	// longer gap is the poll having been asleep -- nothing subscribed, and no
	// bound on how long -- and holding the reading on across that reports the
	// speaker as playing for a delay after Home Assistant returns.
	if gapped {
		s.soundSince, s.soundLastOn = time.Time{}, now
	}
	if playing {
		s.soundLastOn = now
		if s.soundSince.IsZero() {
			s.soundSince = now
		}
		if now.Sub(s.soundSince) >= s.onDelay {
			s.soundOn = true
		}
	} else {
		s.soundSince = time.Time{}
		if s.soundOn && now.Sub(s.soundLastOn) >= s.offDelay {
			s.soundOn = false
		}
	}
	return reading{key: s.keySound, value: boolValue(s.soundOn), ok: true, binary: true}
}

// The heavy tick's readings, taken together because they are published
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
	// Twice the interval sound is sampled on: a sample that late means one was
	// missed, and anything less is the jitter of an ordinary tick. Written
	// under the lock because a test reads it from outside this goroutine. What
	// makes readSound's own read of it safe is not the lock: this goroutine is
	// its only writer, and readSound runs on it.
	s.mu.Lock()
	s.soundGap = 2 * every * SoundEvery
	s.mu.Unlock()
	tick := time.NewTicker(every)
	defer tick.Stop()
	var last time.Time
	woken := false
	ticks := 0
	watched := false
	for {
		// One locked look, used by both the edge below and the gate under it,
		// so the two cannot disagree about the same iteration.
		listening := s.anyStateSubscriber()
		if listening != watched {
			if !listening {
				s.forgetSound()
			}
			watched = listening
		}
		if listening && (!woken || time.Since(last) >= s.liveGap) {
			last = time.Now()
			// All of them for a subscriber that has just arrived, rather than
			// making it wait out the counts.
			var readings []reading
			if woken || ticks%SoundEvery == 0 {
				readings = append(readings, s.readSound())
			}
			if woken || ticks%HeavyEvery == 0 {
				readings = append(readings, s.readLive()...)
			}
			if len(readings) > 0 {
				s.publish("live", readings)
			}
			ticks++
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
