package esphome

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

func TestSoundIsReportedOnlyAfterItHasLasted(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	playing := false
	s.sound = func() (bool, bool) { return playing, true }

	if got := s.readSound(); got.value != 0 || !got.ok {
		t.Fatalf("silence read as %v (ok=%v), want 0", got.value, got.ok)
	}

	playing = true
	if got := s.readSound(); got.value != 0 {
		t.Error("sound was reported by the sample that first saw it, so a 0.6s chime would report")
	}

	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("sound lasting longer than the delay was still not reported")
	}

	playing = false
	clock.pass(s.offDelay / 2)
	if got := s.readSound(); got.value != 1 {
		t.Error("sound was withdrawn by the first silent sample, so a pause reads as the end")
	}

	clock.pass(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence lasting longer than the off delay was still reported as sound")
	}
}

func TestABlipDoesNotAccumulate(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	playing := false
	s.sound = func() (bool, bool) { return playing, true }

	for i := 0; i < 5; i++ {
		playing = true
		s.readSound()
		clock.pass(s.onDelay / 2)
		playing = false
		if got := s.readSound(); got.value != 0 {
			t.Fatalf("blip %d reported sound; the delay is accumulating across gaps", i)
		}
	}
}

func TestAnUnreadableSoundIsMissingAndResetsTheClock(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	state, ok := true, true
	s.sound = func() (bool, bool) { return state, ok }

	s.readSound()
	clock.pass(s.onDelay / 2)

	ok = false
	got := s.readSound()
	if got.ok {
		t.Error("a read that failed was sent as a reading rather than as missing")
	}
	if got.value != 0 {
		t.Errorf("a failed read carried a value of %v", got.value)
	}

	ok = true
	s.readSound()
	clock.pass(s.onDelay/2 + 20*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("the clock survived a failed reading, so a gap counts as sound")
	}
}

func TestAFailedReadDoesNotBringTheWithdrawalForward(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	playing, ok := true, true
	s.sound = func() (bool, bool) { return playing, ok }

	s.readSound()
	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatalf("sound was not reported after the on delay, so there is nothing to withdraw")
	}

	ok = false
	if got := s.readSound(); got.ok {
		t.Error("a read that failed was sent as a reading rather than as missing")
	}

	ok, playing = true, false
	clock.pass(s.offDelay / 2)
	if got := s.readSound(); got.value != 1 {
		t.Error("the silent sample after a failed read withdrew it at once; the off delay was " +
			"measured from the zero time rather than from the last sighting of sound")
	}

	clock.pass(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay was still reported as sound")
	}
}

func TestTheShippedDelaysIgnoreTheChimeAndReportSpeech(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	if s.onDelay != SoundOnDelay || s.offDelay != SoundOffDelay {
		t.Fatalf("a new server has delays of %v/%v, want the shipped %v/%v",
			s.onDelay, s.offDelay, SoundOnDelay, SoundOffDelay)
	}
	const sample = 500 * time.Millisecond
	s.soundGap = 2 * sample
	playing := false
	s.sound = func() (bool, bool) { return playing, true }

	run := func(sounding bool, d time.Duration) float32 {
		playing = sounding
		var last float32
		for spent := time.Duration(0); spent < d; spent += sample {
			clock.pass(sample)
			last = s.readSound().value
		}
		return last
	}

	if worst := SoundOnDelay + sample; worst != 1500*time.Millisecond {
		t.Errorf("the longest sound that can go unreported is %v; README promises about a "+
			"second and a half", worst)
	}

	if got := run(true, 600*time.Millisecond); got != 0 {
		t.Error("a chime of the length this Dot plays was reported as the speaker playing")
	}
	run(false, 2*time.Second)

	if got := run(true, 2*time.Second); got != 1 {
		t.Error("two seconds of sound was not reported; the on delay is longer than it ships")
	}
	if got := run(false, 500*time.Millisecond); got != 1 {
		t.Error("half a second of silence withdrew it; the off delay is shorter than it ships")
	}
	if got := run(false, 1500*time.Millisecond); got != 0 {
		t.Error("two seconds of silence was still reported as sound; the off delay is " +
			"longer than it ships")
	}
}

func TestASamplingGapDoesNotDecideAnEdgeOnItsOwn(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	playing = false
	clock.pass(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("the first silent sample after a gap withdrew it; the gap was counted as silence")
	}
	clock.pass(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay after a gap was still reported as sound")
	}

	playing = true
	s.readSound()
	clock.pass(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("one sample after a gap reported sound; the on clock measured across the gap")
	}
}

func TestASamplingGapDoesNotWithdrawSoundThatIsStillPlaying(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	clock.pass(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("a gap withdrew sound that was still playing on both sides of it")
	}

	playing = false
	clock.pass(s.offDelay / 2)
	if got := s.readSound(); got.value != 1 {
		t.Error("the first silent sample after the gap withdrew it at once; the withdrawal " +
			"was measured from before the gap")
	}
	clock.pass(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay was still reported as sound")
	}
}

func TestAGapAcrossAFailedReadDoesNotReportSoundAgain(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing, ok := true, true
	s.sound = func() (bool, bool) { return playing, ok }

	s.readSound()
	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	playing = false
	ok = false
	clock.pass(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.ok {
		t.Fatal("the failed read was sent as a reading")
	}

	ok = true
	if got := s.readSound(); got.value != 0 {
		t.Error("the first working sample after the failed read reported sound; the " +
			"withdrawal's clock was carried forward across a read that saw nothing")
	}
	if got := s.readSound(); got.value != 0 {
		t.Error("the entity went back on after the failed read")
	}
}

func TestResumingAfterNobodyWasListeningForgetsTheReading(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	clock := soundClock(s)
	shortSoundDelays(s)
	s.soundGap = 100 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	s.forgetSound()
	playing = false
	if got := s.readSound(); got.value != 0 {
		t.Error("the first sample after nobody was listening still reported sound; the reading " +
			"was carried across a stretch nothing was watching")
	}

	playing = true
	s.readSound()
	clock.pass(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound after the resume was never reported at all")
	}
	s.forgetSound()
	if got := s.readSound(); got.value != 0 {
		t.Error("the first sample after nobody was listening reported sound at once, without " +
			"the on delay; the reading was resumed rather than made again")
	}
}

func TestThePollForgetsTheReadingWhenTheLastSubscriberGoes(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	s.sound = func() (bool, bool) { return true, true }
	s.cpu = func() (float32, bool) { return 41.3, true }
	s.memory = func() (float32, bool) { return 126.5, true }
	s.jack = func() (bool, bool) { return true, true }
	s.volumes = func() device.MusicVolume {
		return device.MusicVolume{
			Max: 30, Speaker: 40, SpeakerStep: 12, SpeakerOK: true,
			Jack: 70, JackStep: 21, JackOK: true,
		}
	}

	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	sub := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame, 32), states: true}
	s.mu.Lock()
	s.conns[sub] = struct{}{}
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		delete(s.conns, sub)
		s.mu.Unlock()
	}
	t.Cleanup(drop)

	go s.PollLive(5 * time.Millisecond)

	soundState := func() (reading, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.published[s.keySound]
		return r, ok
	}
	until := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if done() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal(what)
	}

	until("the poll never reported the sound it was told was playing", func() bool {
		r, ok := soundState()
		return ok && r.value == 1
	})

	drop()
	until("the last subscriber went and the poll kept the reading it took for them; a Home "+
		"Assistant that returns is answered from it before anything fresh is read",
		func() bool {
			_, ok := soundState()
			return !ok
		})
}

func shortSoundDelays(s *Server) {
	s.onDelay, s.offDelay = 30*time.Millisecond, 60*time.Millisecond
}

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func (c *fakeClock) pass(d time.Duration) { c.at = c.at.Add(d) }

func soundClock(s *Server) *fakeClock {
	c := &fakeClock{at: time.Unix(1_000_000, 0)}
	s.clock = c.now
	return c
}

func TestARouteChangeWakesTheNameRead(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	wired := s.volumes()
	wired.BluetoothRoute = false
	bluetooth := wired
	bluetooth.BluetoothRoute = true
	s.volumes = func() device.MusicVolume { return wired }

	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("the first route seen woke the name read; subscribing already wakes it")
	}
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("a route that did not change woke the poll that reads the speaker's name")
	}

	s.jack = func() (bool, bool) { return false, true }
	s.readLive()
	s.jack = func() (bool, bool) { return true, true }
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("the route moved between the jack and the speaker, which cannot change the " +
			"Bluetooth device, and woke the name read")
	}

	unknown := wired
	unknown.BluetoothRouteOK = false
	s.volumes = func() device.MusicVolume { return unknown }
	s.readLive()
	s.volumes = func() device.MusicVolume { return wired }
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("a route that could not be read, between two that agree, woke the name read")
	}

	s.volumes = func() device.MusicVolume { return bluetooth }
	s.readLive()
	if len(s.sensorWake) != 1 {
		t.Fatal("the route became bluetooth and the name read was left to the minute tick")
	}
	<-s.sensorWake
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Fatal("a speaker still connected woke the name read on every heavy tick")
	}

	s.volumes = func() device.MusicVolume { return wired }
	s.readLive()
	if len(s.sensorWake) != 1 {
		t.Fatal("the speaker went away and the name it left behind was not read again")
	}
	<-s.sensorWake

	s.volumes = func() device.MusicVolume { return unknown }
	s.readLive()
	s.volumes = func() device.MusicVolume { return bluetooth }
	s.readLive()
	if len(s.sensorWake) != 1 {
		t.Fatal("a route that could not be read hid the speaker that connected across it")
	}

	s.volumes = func() device.MusicVolume { return wired }
	done := make(chan struct{})
	go func() {
		s.readLive()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a wake the sensor poll had not yet taken blocked the live poll")
	}
}

func TestTheRouteIsForgottenWhenTheLastSubscriberGoes(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	wired := s.volumes()
	var onBluetooth atomic.Bool
	s.volumes = func() device.MusicVolume {
		v := wired
		v.BluetoothRoute = onBluetooth.Load()
		return v
	}
	sub := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true}
	join := func() {
		s.mu.Lock()
		s.conns[sub] = struct{}{}
		s.mu.Unlock()
	}
	leave := func() {
		s.mu.Lock()
		delete(s.conns, sub)
		s.mu.Unlock()
	}
	t.Cleanup(leave)
	published := func(key uint32) (reading, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.published[key]
		return r, ok
	}
	until := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if done() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal(what)
	}

	join()
	go s.PollLive(5 * time.Millisecond)
	until("the poll never published the route", func() bool {
		r, ok := published(s.keyOutput)
		return ok && r.text == routeJack
	})

	leave()
	until("the poll never noticed the last subscriber go", func() bool {
		_, ok := published(s.keySound)
		return !ok
	})
	onBluetooth.Store(true)
	select {
	case <-s.sensorWake:
	default:
	}

	join()
	until("the poll never published the route it found on return", func() bool {
		r, ok := published(s.keyOutput)
		return ok && r.text == routeBluetooth
	})
	if len(s.sensorWake) != 0 {
		t.Error("a subscriber's return woke the name read a second time; subscribing already " +
			"wakes it, and the route it was compared against was taken for somebody else")
	}
}
