package esphome

import (
	"net"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

func TestSoundIsReportedOnlyAfterItHasLasted(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
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

	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("sound lasting longer than the delay was still not reported")
	}

	playing = false
	if got := s.readSound(); got.value != 1 {
		t.Error("sound was withdrawn by the first silent sample, so a pause reads as the end")
	}

	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence lasting longer than the off delay was still reported as sound")
	}
}

func TestABlipDoesNotAccumulate(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	playing := false
	s.sound = func() (bool, bool) { return playing, true }

	for i := 0; i < 5; i++ {
		playing = true
		s.readSound()
		time.Sleep(s.onDelay / 2)
		playing = false
		if got := s.readSound(); got.value != 0 {
			t.Fatalf("blip %d reported sound; the delay is accumulating across gaps", i)
		}
	}
}

func TestAnUnreadableSoundIsMissingAndResetsTheClock(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	state, ok := true, true
	s.sound = func() (bool, bool) { return state, ok }

	s.readSound()
	time.Sleep(s.onDelay / 2)

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
	time.Sleep(s.onDelay/2 + 20*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("the clock survived a failed reading, so a gap counts as sound")
	}
}

func TestAFailedReadDoesNotBringTheWithdrawalForward(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	playing, ok := true, true
	s.sound = func() (bool, bool) { return playing, ok }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatalf("sound was not reported after the on delay, so there is nothing to withdraw")
	}

	ok = false
	if got := s.readSound(); got.ok {
		t.Error("a read that failed was sent as a reading rather than as missing")
	}

	ok, playing = true, false
	if got := s.readSound(); got.value != 1 {
		t.Error("the silent sample after a failed read withdrew it at once; the off delay was " +
			"measured from the zero time rather than from the last sighting of sound")
	}

	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay was still reported as sound")
	}
}

func TestTheShippedDelaysIgnoreTheChimeAndReportSpeech(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
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
			time.Sleep(sample)
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
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	playing = false
	time.Sleep(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("the first silent sample after a gap withdrew it; the gap was counted as silence")
	}
	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay after a gap was still reported as sound")
	}

	playing = true
	s.readSound()
	time.Sleep(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("one sample after a gap reported sound; the on clock measured across the gap")
	}
}

func TestASamplingGapDoesNotWithdrawSoundThatIsStillPlaying(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	time.Sleep(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("a gap withdrew sound that was still playing on both sides of it")
	}

	playing = false
	if got := s.readSound(); got.value != 1 {
		t.Error("the first silent sample after the gap withdrew it at once; the withdrawal " +
			"was measured from before the gap")
	}
	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay was still reported as sound")
	}
}

func TestAGapAcrossAFailedReadDoesNotReportSoundAgain(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing, ok := true, true
	s.sound = func() (bool, bool) { return playing, ok }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	playing = false
	ok = false
	time.Sleep(s.soundGap + 50*time.Millisecond)
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
	shortSoundDelays(s)
	s.soundGap = 100 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
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
	time.Sleep(s.onDelay + 5*time.Millisecond)
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
