package esphome

import (
	"net"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

// The reading is not the sample. A chime measured 0.6 seconds on the Dot, and
// the point of the delay is that it never reaches Home Assistant: what the
// entity answers is whether the device is speaking, not whether a sample caught
// a sound.
func TestSoundIsReportedOnlyAfterItHasLasted(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	playing := false
	s.sound = func() (bool, bool) { return playing, true }

	if got := s.readSound(); got.value != 0 || !got.ok {
		t.Fatalf("silence read as %v (ok=%v), want 0", got.value, got.ok)
	}

	// Sound starts. The first sample that sees it is not enough on its own.
	playing = true
	if got := s.readSound(); got.value != 0 {
		t.Error("sound was reported by the sample that first saw it, so a 0.6s chime would report")
	}

	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("sound lasting longer than the delay was still not reported")
	}

	// It stops. The off delay keeps it on so a gap between words does not
	// withdraw it.
	playing = false
	if got := s.readSound(); got.value != 1 {
		t.Error("sound was withdrawn by the first silent sample, so a pause reads as the end")
	}

	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence lasting longer than the off delay was still reported as sound")
	}
}

// A blip that stops before the delay has to leave nothing behind, or the next
// blip inherits its head start and two chimes in a row report as speech.
func TestABlipDoesNotAccumulate(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

// A read that failed is neither state, and it does not carry the clock either:
// the second starts again when the reading comes back, because a gap says
// nothing about what happened inside it.
func TestAnUnreadableSoundIsMissingAndResetsTheClock(t *testing.T) {

	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	// Half the on delay was spent before the failure. Carrying the clock across
	// would report once the other half has passed; starting it again does not.
	ok = true
	s.readSound()
	time.Sleep(s.onDelay/2 + 20*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("the clock survived a failed reading, so a gap counts as sound")
	}
}

// A read that failed is not a sighting of silence, so it must not bring the
// withdrawal forward. soundLastOn was zeroed here once, which made the off test
// measure against the zero time and pass unconditionally: one failed read then
// took the entity off on the very next sample, which is the flapping the delay
// exists to stop.
func TestAFailedReadDoesNotBringTheWithdrawalForward(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	// Straight from the failure to a silent sample, with no sighting of sound
	// in between to set soundLastOn again. Sound was seen moments ago, so the
	// off delay has not run and the entity has to still be on.
	ok, playing = true, false
	if got := s.readSound(); got.value != 1 {
		t.Error("the silent sample after a failed read withdrew it at once; the off delay was " +
			"measured from the zero time rather than from the last sighting of sound")
	}

	// And it still goes off once silence has actually lasted.
	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay was still reported as sound")
	}
}

// Every other test here shrinks its server's delays, so none of them observes
// the values that ship: both could be wrong by orders of magnitude and the
// suite would pass. This one spends four seconds on the two facts the entity is for -- the
// chime measured at 0.6s on the Dot does not report, and speech does. Every
// duration here is a literal rather than the constant it is checking, because a
// sleep of SoundOnDelay+x moves with the constant and would pass whatever it
// were set to.
func TestTheShippedDelaysIgnoreTheChimeAndReportSpeech(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	if s.onDelay != SoundOnDelay || s.offDelay != SoundOffDelay {
		t.Fatalf("a new server has delays of %v/%v, want the shipped %v/%v",
			s.onDelay, s.offDelay, SoundOnDelay, SoundOffDelay)
	}
	// Armed as PollLive arms it, and sampled as PollLive samples: a test that
	// slept a whole delay between readings would trip the gap guard on every
	// one of them, and would then be measuring the guard while blaming the
	// delays.
	const sample = 500 * time.Millisecond
	s.soundGap = 2 * sample
	playing := false
	s.sound = func() (bool, bool) { return playing, true }

	// Sample every half second for the given time, and answer with the last
	// reading taken.
	run := func(sounding bool, d time.Duration) float32 {
		playing = sounding
		var last float32
		for spent := time.Duration(0); spent < d; spent += sample {
			time.Sleep(sample)
			last = s.readSound().value
		}
		return last
	}

	// What README promises, in the one form a reader can check it against: a
	// sound has to outlast the delay and then the sample that finds it, so the
	// worst case is the two together. The runs below cannot hold that number,
	// because no span between two samples half a second apart falls between a
	// one second delay and a shorter one.
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

// PollLive is serial, so a heavy tick whose fork runs long delays the next
// sound sample. Without noticing that, one sample either side of the gap
// decides an edge on its own, which is the single reading the delays exist to
// avoid. Measured against the real loop this is a tick in five, at the volume
// read's two second budget.
func TestASamplingGapDoesNotDecideAnEdgeOnItsOwn(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	// Above both delays, as it is in production: a second against delays of a
	// second sampled twice a second. A threshold under them would make every
	// ordinary wait for a delay look like a gap.
	s.soundGap = 200 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	// A gap longer than two samples, then silence. The withdrawal has to start
	// its delay again rather than measure across the gap.
	playing = false
	time.Sleep(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Error("the first silent sample after a gap withdrew it; the gap was counted as silence")
	}
	time.Sleep(s.offDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("silence past the off delay after a gap was still reported as sound")
	}

	// And the other edge: a stale on-clock must not let one sample report a
	// sound too short to qualify.
	playing = true
	s.readSound()
	time.Sleep(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.value != 0 {
		t.Error("one sample after a gap reported sound; the on clock measured across the gap")
	}
}

// The bounded gap keeps the reading where the unbounded one drops it. A speaker
// playing across a stalled heavy tick must not flap off and back on, which is
// worse than the gap being guarded against -- and that is the whole difference
// between this and forgetSound.
func TestASamplingGapDoesNotWithdrawSoundThatIsStillPlaying(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	// And the clocks did start again, so the withdrawal is measured from the
	// gap rather than from before it: silence now takes a full delay.
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

// The guard must not carry the withdrawal's clock forward on a sample that read
// nothing. Doing so held the entity on across the failure and then reported it
// playing again afterwards, which Home Assistant renders as the speaker
// resuming: on, unknown, on, off, with that second on arriving seconds after
// the speaker went quiet.
func TestAGapAcrossAFailedReadDoesNotReportSoundAgain(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	s.soundGap = 200 * time.Millisecond
	playing, ok := true, true
	s.sound = func() (bool, bool) { return playing, ok }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	// The speaker stops, and the sample that would have seen it fails after a
	// gap -- a stalled heavy tick, which is where both of these come from.
	playing = false
	ok = false
	time.Sleep(s.soundGap + 50*time.Millisecond)
	if got := s.readSound(); got.ok {
		t.Fatal("the failed read was sent as a reading")
	}

	// From here the readings work again and there is no sound. Sound was last
	// seen before the gap, which is longer ago than the off delay, so the very
	// first working sample has to withdraw it. Reporting sound here is the
	// speaker appearing to resume.
	ok = true
	if got := s.readSound(); got.value != 0 {
		t.Error("the first working sample after the failed read reported sound; the " +
			"withdrawal's clock was carried forward across a read that saw nothing")
	}
	if got := s.readSound(); got.value != 0 {
		t.Error("the entity went back on after the failed read")
	}
}

// With no subscriber PollLive stops sampling entirely, and nothing bounds how
// long that lasts: Home Assistant restarting is one gap, and it can be an hour.
// Carrying the reading across reports the speaker as playing for a delay after
// Home Assistant returns, and fires anything triggered on it turning on. This is
// the unbounded case, and unlike the sampling gap inside readSound it drops the
// reading rather than only the clocks.
func TestResumingAfterNobodyWasListeningForgetsTheReading(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	s.soundGap = 100 * time.Millisecond
	playing := true
	s.sound = func() (bool, bool) { return playing, true }

	s.readSound()
	time.Sleep(s.onDelay + 5*time.Millisecond)
	if got := s.readSound(); got.value != 1 {
		t.Fatal("sound was not reported after the on delay")
	}

	// Nobody is listening for a while; the Dot goes quiet meanwhile.
	s.forgetSound()
	playing = false
	if got := s.readSound(); got.value != 0 {
		t.Error("the first sample after nobody was listening still reported sound; the reading " +
			"was carried across a stretch nothing was watching")
	}

	// And the other way round: the Dot is playing when Home Assistant returns.
	// That is a reading to make afresh, not one to resume, so it still has to
	// last the on delay before it is reported.
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

// PollLive is what notices, and it has to notice the edge rather than the
// state: the last subscriber going is when the reading stops meaning anything,
// because nothing bounds the stretch that follows. Observed through the
// published state rather than the clocks, because those belong to the poll's
// own goroutine and reading them from here is the race the comment on them
// warns about.
func TestThePollForgetsTheReadingWhenTheLastSubscriberGoes(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	shortSoundDelays(s)
	s.sound = func() (bool, bool) { return true, true }
	// This poll is left running at 5ms for the rest of the package, so every
	// other reading it takes has to be steady: the real ones answer from /proc
	// where the tests run, and a memory figure that moves on every read fills a
	// subscriber's queue and has it dropped mid-test.
	s.cpu = func() (float32, bool) { return 41.3, true }
	s.memory = func() (float32, bool) { return 126.5, true }
	s.jack = func() (bool, bool) { return true, true }
	s.volumes = func() device.MusicVolume {
		return device.MusicVolume{Speaker: 40, SpeakerOK: true, Jack: 70, JackOK: true}
	}

	// A real address, because a send that fails takes eachConn through the
	// address of the connection it drops.
	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	sub := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame, 32), states: true}
	s.mu.Lock()
	s.conns[sub] = struct{}{}
	s.mu.Unlock()
	// Dropped whatever happens, so the poll this test cannot stop goes idle
	// rather than publishing to a subscriber nothing is reading for the rest of
	// the run.
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

// Shrunk on the server rather than in the package, so one test's delays cannot
// reach another test's poll.
func shortSoundDelays(s *Server) {
	s.onDelay, s.offDelay = 30*time.Millisecond, 60*time.Millisecond
}
