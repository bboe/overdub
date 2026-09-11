package esphome

import (
	"fmt"
	"log"
	"strconv"
	"time"
)

const (
	kindSensor = iota
	kindBinary
	kindSelect
	kindSwitch
	kindMedia
)

type reading struct {
	key   uint32
	value float32
	text  string
	muted bool
	ok    bool
	kind  int
}

func (s *Server) readTicked() []reading {
	up, upOK := s.uptime()
	signal, signalOK := s.wifi()
	out := []reading{
		{key: s.keyUptime, value: up, ok: upOK},
		{key: s.keyWifi, value: signal, ok: signalOK},
	}
	for _, b := range s.buttons {
		out = append(out, reading{key: b.keyMode, text: b.mode(), ok: true, kind: kindSelect})
	}
	if mode, known := s.adbMode(); known {
		s.adbObserved(mode)
		out = append(out, reading{key: s.keyADB, text: mode.String(), ok: true, kind: kindSelect})
	}
	return out
}

const (
	SoundOnDelay  = time.Second
	SoundOffDelay = time.Second
)

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
	gapped := s.soundGap > 0 && !s.soundSeen.IsZero() && now.Sub(s.soundSeen) > s.soundGap
	s.soundSeen = now
	if !ok {
		s.soundSince = time.Time{}
		return reading{key: s.keySound, kind: kindBinary}
	}
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
	return reading{key: s.keySound, value: boolValue(s.soundOn), ok: true, kind: kindBinary}
}

func (s *Server) readLive() []reading {
	cpu, cpuOK := s.cpu()
	memory, memoryOK := s.memory()
	volumes := s.volumes()
	occupied, jackOK := s.jack()
	muted, micOK := s.micMute()
	out := []reading{
		{key: s.keyCPU, value: cpu, ok: cpuOK},
		{key: s.keyMemory, value: memory, ok: memoryOK},
		{key: s.keyVolume, value: volumes.Speaker, ok: volumes.SpeakerOK},
		{key: s.keyJack, value: volumes.Jack, ok: volumes.JackOK},
		{key: s.keyJackOn, value: boolValue(occupied), ok: jackOK, kind: kindBinary},
	}
	if step, _, ok := activeVolume(volumes, occupied, jackOK); ok && volumes.Max > 0 {
		out = append(out, reading{
			key:   s.keySpeaker,
			value: float32(step) / float32(volumes.Max),
			muted: volumes.Muted,
			ok:    true,
			kind:  kindMedia,
		})
	}
	if micOK {
		s.micObserved(muted)
		out = append(out, reading{key: s.keyMicMute, value: boolValue(muted), ok: true, kind: kindSwitch})
	}
	return out
}

func boolValue(b bool) float32 {
	if b {
		return 1
	}
	return 0
}

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
		switch r.kind {
		case kindBinary:
			msgType, payload = msgBinarySensorState, binaryState(r.key, r.value != 0, !r.ok)
		case kindSelect:
			msgType, payload = msgSelectState, selectState(r.key, r.text)
		case kindSwitch:
			msgType, payload = msgSwitchState, switchState(r.key, r.value != 0)
		case kindMedia:
			msgType, payload = msgMediaPlayerState, mediaState(r.key, r.value, r.muted)
		}
		if err := s.send(conn, msgType, payload); err != nil {
			return err
		}
	}
	return nil
}

func selectState(key uint32, choice string) []byte {
	var p pb
	p.fixed32(1, key)
	p.str(2, choice)
	return p.b
}

func switchState(key uint32, on bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(2, on)
	return p.b
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

func wantsServices(c *conn) bool { return c.services }

func (s *Server) publish(what string, readings []reading) []reading {
	s.mu.Lock()
	var changed []reading
	for _, r := range readings {
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

func (s *Server) FirePress(objectID string, eventType EventType, count int, holdFor time.Duration) {
	b := s.button(objectID)
	if b == nil {
		return
	}
	var event pb
	event.fixed32(1, b.keyEvent)
	event.str(2, string(eventType))

	var action pb
	action.str(1, "esphome.overdub_pressed")
	action.sub(2, kv("event_type", string(eventType)))
	action.sub(2, kv("device", s.name))
	action.sub(2, kv("button", b.objectID))
	if count > 0 {
		action.sub(3, kv("multi_press_count", strconv.Itoa(count)))
	}
	if holdFor > 0 {
		action.sub(3, kv("held_ms", strconv.FormatInt(holdFor.Milliseconds(), 10)))
	}
	action.boolean(5, true) // is_event

	s.mu.Lock()
	failed := s.eachConn("event", wantsStates, func(c *conn) error {
		return s.send(c, msgEventState, event.b)
	})
	failed = append(failed, s.eachConn("event count", wantsServices, func(c *conn) error {
		return s.send(c, msgHomeassistantAct, action.b)
	})...)
	s.mu.Unlock()
	for _, line := range failed {
		s.peerLogf("%s", line)
	}
}

func kv(key, value string) []byte {
	var p pb
	p.str(1, key)
	p.str(2, value)
	return p.b
}

func (s *Server) Poll(sensorTick, liveTick time.Duration) {
	go s.PollSensors(sensorTick)
	go s.PollLive(liveTick)
}

func (s *Server) PollSensors(every time.Duration) {
	if every < MinSensorTick {
		log.Printf("esphome api: sensor tick of %v raised to the %v floor", every, MinSensorTick)
		every = MinSensorTick
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if !s.adbBusy() {
			_ = s.adbHold()
		}
		s.publish("sensors", s.readTicked())
		read := time.Now()
		select {
		case <-tick.C:
		case <-s.sensorWake:
			if left := s.wakeGap - time.Since(read); left > 0 {
				time.Sleep(left)
			}
		}
	}
}

func (s *Server) PollLive(every time.Duration) {
	if every <= 0 {
		log.Printf("esphome api: live tick of %v raised to %v", every, minLiveReadGap)
		every = minLiveReadGap
	}
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
		listening := s.anyStateSubscriber()
		if listening != watched {
			if !listening {
				s.forgetSound()
			}
			watched = listening
		}
		if listening && (!woken || time.Since(last) >= s.wakeGap) {
			last = time.Now()
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

func (s *Server) stateSubscriberBesides(except *conn) bool {
	for conn := range s.conns {
		if conn != except && conn.states {
			return true
		}
	}
	return false
}
