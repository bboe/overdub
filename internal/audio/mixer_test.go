package audio

import (
	"errors"
	"math"
	"testing"
)

func steady(level int16, frames int) *clip {
	pcm := make([]int16, frames)
	for i := range pcm {
		pcm[i] = level
	}
	return &clip{pcm: pcm}
}

func TestAnIdleMixerAsksForNothingToBeWritten(t *testing.T) {
	m := newMixer()
	if m.next(make([]int16, BlockFrames)) {
		t.Error("a mixer with no source still wants blocks written, which holds the amp awake")
	}
}

func TestOneSourcePassesThroughUntouched(t *testing.T) {
	m := newMixer()
	m.start(steady(1000, BlockFrames))

	block := make([]int16, BlockFrames)
	if !m.next(block) {
		t.Fatal("a mixer with a source reported nothing to play")
	}
	for i, s := range block {
		if s != 1000 {
			t.Fatalf("sample %d is %d, want the source's own 1000", i, s)
		}
	}
}

func TestTwoSourcesAreSummed(t *testing.T) {
	m := newMixer()
	m.start(steady(1000, BlockFrames))
	m.start(steady(-250, BlockFrames))

	block := make([]int16, BlockFrames)
	if !m.next(block) {
		t.Fatal("a mixer with two sources reported nothing to play")
	}
	if block[0] != 750 {
		t.Errorf("the first sample is %d, want 1000 + -250", block[0])
	}
}

func TestSummingSaturatesRatherThanWrapping(t *testing.T) {
	m := newMixer()
	m.start(steady(30000, BlockFrames))
	m.start(steady(30000, BlockFrames))

	block := make([]int16, BlockFrames)
	m.next(block)
	if block[0] != math.MaxInt16 {
		t.Errorf("30000 + 30000 came out as %d; wrapping past the top of int16 is a"+
			" full-scale crack rather than a loud chime", block[0])
	}

	m = newMixer()
	m.start(steady(-30000, BlockFrames))
	m.start(steady(-30000, BlockFrames))
	m.next(block)
	if block[0] != math.MinInt16 {
		t.Errorf("-30000 + -30000 came out as %d, want the bottom of int16", block[0])
	}
}

func TestAShorterSourceOnlyReachesItsOwnSamples(t *testing.T) {
	m := newMixer()
	m.start(steady(500, 10))
	m.start(steady(700, BlockFrames))

	block := make([]int16, BlockFrames)
	m.next(block)
	if block[0] != 1200 {
		t.Errorf("the first sample is %d, want both sources", block[0])
	}
	if block[10] != 700 {
		t.Errorf("sample 10 is %d; the shorter source was summed past its own end,"+
			" or left stale samples behind it", block[10])
	}
}

func TestASourceIsDroppedOnceItIsSpent(t *testing.T) {
	m := newMixer()
	m.start(steady(500, BlockFrames))

	block := make([]int16, BlockFrames)
	if !m.next(block) {
		t.Fatal("the first block was refused")
	}
	if m.next(block) {
		t.Error("a spent source still asks for blocks, so the writer never goes idle")
	}
}

func TestStartingASoundAlreadyPlayingRewindsItRatherThanDoublingIt(t *testing.T) {
	m := newMixer()
	c := steady(1000, 2*BlockFrames)
	m.start(c)

	block := make([]int16, BlockFrames)
	m.next(block)

	m.start(c)
	if !m.next(block) {
		t.Fatal("the restarted sound reported nothing to play")
	}
	if block[0] != 1000 {
		t.Errorf("the first sample after a restart is %d, want the source once at 1000",
			block[0])
	}
	if !m.next(block) {
		t.Fatal("the restarted sound ended early, so start did not rewind it")
	}
}

func TestBlockBytesSurviveTheRoundTrip(t *testing.T) {
	block := []int16{0, 1, -1, math.MaxInt16, math.MinInt16, 1234}
	buf := make([]byte, len(block)*2)
	encode(block, buf)
	for i, got := range decode(buf) {
		if got != block[i] {
			t.Errorf("sample %d came back as %d, want %d", i, got, block[i])
		}
	}
}

func TestABlockIsTenMillisecondsOfTheFormatTheChimeDeclares(t *testing.T) {
	if ms := BlockFrames * 1000 / ChimeRate; ms != 10 {
		t.Errorf("a block is %d ms at %d Hz, want 10", ms, ChimeRate)
	}
	if BlockBytes != BlockFrames*ChimeChannels*2 {
		t.Errorf("a block is %d bytes against %d frames of %d channel 16-bit audio",
			BlockBytes, BlockFrames, ChimeChannels)
	}
}

func TestWriteAllHandsOverEveryByteInOrder(t *testing.T) {
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}
	var got []byte
	err := writeAll(want, func(buf []byte) (int, error) {
		n := min(4, len(buf))
		got = append(got, buf[:n]...)
		return n, nil
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("writeAll: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the player was handed %v, want %v; a block larger than one buffer"+
			" has to be written across several", got, want)
	}
}

func TestWriteAllWaitsOutAFullQueue(t *testing.T) {
	refusals, waits := 2, 0
	err := writeAll([]byte{1, 2}, func(buf []byte) (int, error) {
		if refusals > 0 {
			refusals--
			return 0, nil
		}
		return len(buf), nil
	}, func() bool { waits++; return true })
	if err != nil {
		t.Fatalf("writeAll: %v", err)
	}
	if waits != 2 {
		t.Errorf("the writer waited %d times for a queue that refused twice", waits)
	}
}

func TestWriteAllStopsWhenTheWaitIsOver(t *testing.T) {
	writes := 0
	err := writeAll([]byte{1, 2}, func([]byte) (int, error) {
		writes++
		return 0, nil
	}, func() bool { return false })
	if !errors.Is(err, errStopping) {
		t.Errorf("writeAll returned %v, want the closing sentinel, which is not a"+
			" failure worth logging", err)
	}
	if writes != 1 {
		t.Errorf("the player was written to %d times after the close", writes)
	}
}

func TestWriteAllReportsWhatThePlayerReported(t *testing.T) {
	want := errors.New("audio_write would not take the block")
	err := writeAll([]byte{1, 2}, func([]byte) (int, error) { return 0, want },
		func() bool { return true })
	if !errors.Is(err, want) {
		t.Errorf("writeAll returned %v, want the player's own error", err)
	}
}

func TestWriteAllRefusesAPlayerClaimingMoreThanItWasOffered(t *testing.T) {
	err := writeAll([]byte{1, 2}, func(buf []byte) (int, error) { return len(buf) + 1, nil },
		func() bool { return true })
	if err == nil {
		t.Error("a count past the end of the block was believed, which slices out of range")
	}
}

func TestWriteAllRefusesAPlayerThatReportsANegativeCount(t *testing.T) {
	err := writeAll([]byte{1, 2}, func([]byte) (int, error) { return -1, nil },
		func() bool { return true })
	if err == nil {
		t.Error("a negative count was believed, which slices with a negative index and" +
			" panics the daemon into the supervisor's restart loop")
	}
}

func TestAShortBlockIsFilledRatherThanOverrun(t *testing.T) {
	m := newMixer()
	m.start(steady(300, BlockFrames))

	block := make([]int16, 8)
	if !m.next(block) {
		t.Fatal("a short block was refused")
	}
	for i, s := range block {
		if s != 300 {
			t.Fatalf("sample %d of a short block is %d, want the source's own 300", i, s)
		}
	}
}

func TestWriteAllGivesUpOnAQueueThatNeverDrains(t *testing.T) {
	writes, waits := 0, 0
	err := writeAll([]byte{1, 2}, func([]byte) (int, error) { writes++; return 0, nil },
		func() bool { waits++; return true })
	if err == nil {
		t.Fatal("a queue that never drains is waited on for ever, which is silence with" +
			" nothing logged and nothing to distinguish it from an ordinary full queue")
	}
	if errors.Is(err, errStopping) {
		t.Error("giving up on a wedged queue reads as an ordinary close, so it is never logged")
	}
	if writes > stuckWaits+2 {
		t.Errorf("the player was written to %d times before giving up, want about %d",
			writes, stuckWaits)
	}
}

func TestALongBlockIsFilledRatherThanPaddedWithSilence(t *testing.T) {
	m := newMixer()
	m.start(steady(400, 4*BlockFrames))

	block := make([]int16, 2*BlockFrames)
	if !m.next(block) {
		t.Fatal("a long block was refused")
	}
	for i, s := range block {
		if s != 400 {
			t.Fatalf("sample %d of a long block is %d; the tail past one block's worth"+
				" is silence padded onto real audio", i, s)
		}
	}
}

func TestAMixerWithNothingToPlayIsNotSounding(t *testing.T) {
	m := newMixer()
	if m.sounding() {
		t.Error("a mixer with no source reported that ours was playing, so the shared" +
			" queue Alexa fills would be read as our own")
	}
	m.start(steady(500, BlockFrames))
	if !m.sounding() {
		t.Error("a mixer with a source reported nothing playing")
	}
	m.next(make([]int16, BlockFrames))
	if m.sounding() {
		t.Error("a spent source still counts as ours playing, so a position taken after" +
			" the sound ended is measured against whatever plays next")
	}
}
