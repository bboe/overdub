package audio

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func quiet() *Stream { return &Stream{say: func(string, ...any) {}} }

func anchorAt(s *Stream, when time.Time) {
	s.first = time.Now().Add(-anchorSettle)
	for range anchorTake {
		s.observe(Point{At: when})
	}
	if !s.anchored {
		panic("the stream did not learn where the player had reached")
	}
}

func level(frames int, at int16) []byte {
	block := make([]int16, frames)
	for i := range block {
		block[i] = at
	}
	buf := make([]byte, frames*frameBytes)
	encode(block, buf)
	return buf
}

func ramp(from, frames int) []byte {
	block := make([]int16, frames)
	for i := range block {
		block[i] = int16((from + i) % 30000)
	}
	buf := make([]byte, frames*frameBytes)
	encode(block, buf)
	return buf
}

func TestAudioArrivingBeforeTheMappingWaitsForItRatherThanBeingSpent(t *testing.T) {
	s := quiet()
	now := time.Now()
	if err := s.Write(now.Add(200*time.Millisecond), level(BlockFrames, 5000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	for b := range anchorTake {
		if n, more := s.read(block); n != BlockFrames || !more {
			t.Fatalf("read returned %d, %v; an open stream keeps the writer fed", n, more)
		}
		for i, got := range block {
			if got != 0 {
				t.Fatalf("block %d sample %d is %d; audio was placed before anything said"+
					" which frame the player was on", b, i, got)
			}
		}
		s.observe(Point{At: now.Add(time.Duration(b) * 10 * time.Millisecond)})
	}
	if !s.anchored {
		t.Fatal("the stream took a reading after every block and still has no mapping")
	}
	if s.held != BlockFrames {
		t.Fatalf("%d frames are left of the block written before the mapping existed;"+
			" audio a server sent at stream/start was spent against a mapping that was"+
			" not there yet", s.held)
	}
}

func TestNoReadingIsTakenUntilThePipelineIsSteady(t *testing.T) {
	s := quiet()
	block := make([]int16, BlockFrames)
	for range 20 {
		s.read(block)
		if s.wants() {
			t.Fatalf("a reading was taken inside the first %s of a stream; the writer"+
				" fills the whole queue at once there, so the HAL buffer is still"+
				" filling and the origin comes out about 27 ms early", anchorSettle)
		}
	}
	s.first = time.Now().Add(-anchorSettle)
	if !s.wants() {
		t.Errorf("no reading was wanted after %s, so the stream never learns where the"+
			" player has reached and plays nothing but silence", anchorSettle)
	}
}

func TestAStreamKeepsAskingUntilItIsPlacedAndSparinglyAfterwards(t *testing.T) {
	s := quiet()
	s.first = time.Now().Add(-anchorSettle)
	for i := range 3 {
		if !s.wants() {
			t.Fatalf("reading %d was not wanted while the stream had no mapping, so it"+
				" waits %d blocks to learn one and drops the audio due in between",
				i, observeEvery)
		}
		s.observe(Point{At: time.Now()})
	}
	anchorAt(s, time.Now())
	if s.wants() {
		t.Error("a placed stream asked for another reading at once; the status file costs" +
			" about 300 us of every 10 ms block")
	}
	block := make([]int16, BlockFrames)
	for range observeEvery {
		s.read(block)
	}
	if !s.wants() {
		t.Errorf("a placed stream never asked again, so a queue that stopped draining"+
			" is never noticed; it should ask every %d blocks", observeEvery)
	}
}

func TestTheMappingIsTheMiddleReadingRatherThanTheFirstOrTheLast(t *testing.T) {
	s := quiet()
	now := time.Now()
	for _, off := range []time.Duration{-9 * time.Millisecond, 7 * time.Millisecond,
		0, 0, 0} {
		s.observe(Point{At: now.Add(off)})
	}
	if !s.anchored {
		t.Fatalf("the stream took %d readings and still had no mapping", anchorTake)
	}
	if slip := s.origin.Sub(now); slip != 0 {
		t.Errorf("frame 0 was placed %s from where the honest readings put it; an outlier"+
			" reached the mapping rather than being outvoted", slip)
	}
}

func TestAudioIsPlacedAtTheFrameItsTimestampNames(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)

	if err := s.Write(now.Add(25*time.Millisecond), level(1200, 4000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	for b := range 2 {
		s.read(block)
		for i, got := range block {
			if got != 0 {
				t.Fatalf("block %d sample %d is %d; audio due 25 ms out was played in the"+
					" first %d ms, which is that far ahead of the rest of the group",
					b, i, got, (b+1)*10)
			}
		}
	}
	s.read(block)
	for i, got := range block {
		want := int16(0)
		if i >= 1200-2*BlockFrames {
			want = 4000
		}
		if got != want {
			t.Fatalf("frame %d is %d, want %d: the chunk starts at frame 1200, which is"+
				" 240 frames into the third block", 2*BlockFrames+i, got, want)
		}
	}
}

func TestChunksThatDoNotDivideIntoBlocksStayContinuous(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)

	const chunk = 1200
	for i := range 4 {
		at := now.Add(time.Duration(i) * 25 * time.Millisecond)
		if err := s.Write(at, ramp(i*chunk, chunk)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	block := make([]int16, BlockFrames)
	for b := range 4 * chunk / BlockFrames {
		s.read(block)
		for i, got := range block {
			frame := b*BlockFrames + i
			if want := int16(frame % 30000); got != want {
				t.Fatalf("frame %d is %d, want %d: a chunk boundary inside a block either"+
					" repeated a frame or left a hole, and at 16 bits every later frame"+
					" is then paired with the wrong neighbour", frame, got, want)
			}
		}
	}
}

func TestAChunkAlreadyPastIsDroppedRatherThanPlayedLate(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now.Add(-100*time.Millisecond), level(1200, 6000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	s.read(block)
	for i, got := range block {
		if got != 0 {
			t.Fatalf("sample %d is %d; audio whose moment had passed was played anyway,"+
				" so everything after it is late by as much", i, got)
		}
	}
	if s.late != 1200 {
		t.Errorf("%d frames were counted late, want the whole 1,200-frame chunk", s.late)
	}
}

func TestAChunkThatStartedInThePastPlaysTheRestOfItselfInPlace(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now.Add(-5*time.Millisecond), ramp(0, 1200)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	s.read(block)
	for i, got := range block {
		if want := int16(240 + i); got != want {
			t.Fatalf("frame %d is %d, want %d: the part of a chunk that is still due has"+
				" to play where it belongs rather than from the chunk's own start",
				i, got, want)
		}
	}
	if s.late != 240 {
		t.Errorf("%d frames were counted late, want the 240 that were already past", s.late)
	}
}

func TestAGapIsSilenceRatherThanTheNextChunkPulledForward(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now.Add(30*time.Millisecond), level(480, 7000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	for b := range 3 {
		s.read(block)
		for i, got := range block {
			if got != 0 {
				t.Fatalf("block %d sample %d is %d; a chunk due 30 ms out filled the hole"+
					" in front of it, so every frame after the gap plays early", b, i, got)
			}
		}
	}
	s.read(block)
	if block[0] != 7000 {
		t.Errorf("the chunk due at 30 ms came out as %d at the fourth block", block[0])
	}
	if s.silence != 3*BlockFrames {
		t.Errorf("%d frames of silence were counted over three empty blocks, want %d",
			s.silence, 3*BlockFrames)
	}
}

func TestTheStreamHoldsABoundedAmountOfAudio(t *testing.T) {
	s := quiet()
	now := time.Now()
	for i := range streamHold / ChimeRate {
		if err := s.Write(now, level(ChimeRate, 100)); err != nil {
			t.Fatalf("second %d of %s: %v", i, frameTime(streamHold), err)
		}
	}
	if err := s.Write(now, level(1, 100)); err == nil {
		t.Errorf("a server can buffer past %s here, which is memory a peer chooses the"+
			" size of on a device with 512 MB", frameTime(streamHold))
	}
}

func TestAudioDueBeyondAnyStreamIsRefused(t *testing.T) {
	s := quiet()
	now := time.Now()
	for _, when := range []time.Time{now.Add(streamAhead + time.Second),
		now.Add(-streamAhead - time.Second)} {
		if err := s.Write(when, level(480, 100)); err == nil {
			t.Errorf("audio due %s from now was buffered; a stamp near the %s ceiling"+
				" onAClock allows turns into a frame count that overflows int64 on the"+
				" way to a frame index", when.Sub(now), frameTime(1<<50))
		}
	}
}

func TestAPartialFrameIsRefusedRatherThanPairedWithTheNextWrite(t *testing.T) {
	s := quiet()
	if err := s.Write(time.Now(), []byte{1, 2, 3}); err == nil {
		t.Error("an odd number of bytes was taken, which pairs every later byte with the" +
			" wrong neighbour for as long as the stream runs")
	}
}

func TestClearingAStreamKeepsItRunning(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, level(4*BlockFrames, 8000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Clear()
	block := make([]int16, BlockFrames)
	if n, more := s.read(block); n != BlockFrames || !more {
		t.Fatalf("read returned %d, %v after a clear; the stream is still the one the"+
			" server announced", n, more)
	}
	if block[0] != 0 {
		t.Errorf("the first sample after a clear is %d, want what was buffered thrown away",
			block[0])
	}
	if s.held != 0 {
		t.Errorf("%d frames are still charged against the ceiling after a clear", s.held)
	}
	if err := s.Write(now.Add(time.Second), level(480, 9000)); err != nil {
		t.Errorf("a clear closed the stream to later audio: %v", err)
	}
}

func TestAClosedStreamLetsTheWriterGoIdle(t *testing.T) {
	s := quiet()
	s.Close()
	if n, more := s.read(make([]int16, BlockFrames)); n != 0 || more {
		t.Errorf("read returned %d, %v after a close; the writer keeps feeding silence to"+
			" the player, which holds the amp awake for the life of the daemon", n, more)
	}
	if err := s.Write(time.Now(), level(480, 100)); !errors.Is(err, errStreamClosed) {
		t.Errorf("Write after close returned %v", err)
	}
	s.Close()
}

func TestCloseSaysWhatTheStreamDidWithTheAudio(t *testing.T) {
	var lines [][]any
	s := &Stream{say: func(_ string, args ...any) { lines = append(lines, args) }}
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, level(BlockFrames, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write(now.Add(-time.Second), level(240, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	s.read(block)
	s.read(block)
	s.Close()

	if len(lines) != 1 {
		t.Fatalf("a stream that ended said %d things; the only place the silence it"+
			" inserted and the audio it dropped are readable is this line", len(lines))
	}
	want := []any{frameTime(BlockFrames), frameTime(BlockFrames), frameTime(240), 0}
	for i, got := range lines[0] {
		if got != want[i] {
			t.Errorf("the line reports %v at position %d, want %v: one block of audio"+
				" played, one of silence after it, and a chunk a second late dropped",
				got, i, want[i])
		}
	}
}

func TestASmallSlipIsNotWorthSkippingTheAudioFor(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	origin := s.origin

	s.observe(Point{At: now.Add(anchorSlip / 2)})
	if s.slips != 0 || !s.origin.Equal(origin) {
		t.Errorf("a %s reading moved the mapping; the instrument itself is only good to a"+
			" few milliseconds, so following it skips audio to chase its own noise",
			anchorSlip/2)
	}
}

func TestASlipThatHoldsMovesTheMappingAndThenStartsOver(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)

	moved := now.Add(10 * anchorSlip)
	for range slipRuns {
		s.observe(Point{At: moved})
	}
	if s.slips != 1 || !s.origin.Equal(moved) {
		t.Fatalf("a %s slip that held for %d readings left the mapping at %s, so a"+
			" starved queue leaves the stream that far behind the group for the rest"+
			" of the track", 10*anchorSlip, slipRuns, s.origin.Sub(now))
	}

	s.observe(Point{At: moved.Add(10 * anchorSlip)})
	if s.slips != 1 {
		t.Errorf("the reading after a correction moved the mapping on its own; the run" +
			" has to start over, or every excursion after the first correction is acted" +
			" on immediately and the stream chases the instrument again")
	}
}

func TestPlacingCountsWhatThePlayerStillHolds(t *testing.T) {
	s := quiet()
	s.first = time.Now().Add(-anchorSettle)
	now := time.Now()
	const depth = 6896
	s.observe(Point{Ahead: depth, At: now})
	if len(s.candidates) != 1 {
		t.Fatalf("the reading was not taken")
	}
	if got := s.candidates[0].Sub(now); got != frameTime(depth) {
		t.Errorf("the next frame was placed %s out, want the %s the player and the HAL"+
			" still hold between them", got, frameTime(depth))
	}
}

func TestAPlayerThatWillNotSayWhereItIsSaysSoOnce(t *testing.T) {
	said := 0
	s := &Stream{say: func(string, ...any) { said++ }}
	for range 3 * blindAfter {
		s.blind(errors.New("the output is XRUN rather than running"))
	}
	if said != 1 {
		t.Errorf("a player that never answers drew %d lines; one is the signal and a line"+
			" per block is 100 a second against a budget of 20 a minute", said)
	}
}

func TestAStreamIsWrittenToWhileItIsRead(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		block := make([]int16, BlockFrames)
		for range 200 {
			s.read(block)
			s.wants()
			s.observe(Point{At: time.Now()})
		}
	}()
	for i := range 200 {
		at := now.Add(time.Duration(i) * 25 * time.Millisecond)
		if err := s.Write(at, level(1200, int16(i))); err != nil && i < 40 {
			t.Errorf("write %d: %v", i, err)
		}
		if i%40 == 0 {
			s.Clear()
		}
	}
	<-done
	s.Close()
}

func TestPlacingAStreamSaysHowFarAheadOfTheSpeakerThePlayerIs(t *testing.T) {
	var said []any
	s := &Stream{say: func(_ string, args ...any) { said = append(said, args...) }}
	s.first = time.Now().Add(-anchorSettle)
	const depth = 6896
	for range anchorTake {
		s.observe(Point{Ahead: depth, At: time.Now()})
	}
	if len(said) != 1 {
		t.Fatalf("placing a stream said %d things, want the one line that makes the"+
			" declared required_lead_time_ms checkable on hardware", len(said))
	}
	if got := said[0]; got != frameTime(depth).Round(time.Millisecond) {
		t.Errorf("the line reports %v, want the %s the player and the HAL hold between"+
			" a written frame and the speaker", got, frameTime(depth))
	}
}

func TestAOneOffExcursionDoesNotDragTheMappingThereAndBack(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	origin := s.origin

	s.observe(Point{At: now.Add(2 * anchorSlip)})
	if s.slips != 0 || !s.origin.Equal(origin) {
		t.Fatalf("one reading past the threshold moved the mapping on its own."+
			" Measured on a Dot: the reading swings %s out and back 258 ms later,"+
			" three times over five minutes of music, so acting on one of those is"+
			" two audible skips that cancel each other out", 2*anchorSlip)
	}

	s.observe(Point{At: now})
	for range slipRuns - 1 {
		s.observe(Point{At: now.Add(2 * anchorSlip)})
	}
	if s.slips != 0 {
		t.Error("a reading back inside the threshold did not clear the run, so" +
			" excursions scattered over a track add up to a correction between them")
	}
}

func TestTheSwingThisInstrumentMakesOnItsOwnIsInsideTheThreshold(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	origin := s.origin

	for _, swing := range []time.Duration{21 * time.Millisecond, 22 * time.Millisecond,
		-24 * time.Millisecond} {
		for range 2 * slipRuns {
			s.observe(Point{At: now.Add(swing)})
		}
		if s.slips != 0 || !s.origin.Equal(origin) {
			t.Fatalf("a %s reading moved the mapping. Measured against Music Assistant"+
				" on a Dot, this instrument swings 21, 22 and -24 ms out and back on"+
				" its own, holding it for several readings at a time, so a threshold"+
				" under that corrects nothing and skips the audio twice per episode",
				swing)
		}
	}
}

func TestAFinishedStreamPlaysOutWhatItHoldsBeforeItGoes(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, level(3*BlockFrames, 5000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Finish()

	block := make([]int16, BlockFrames)
	for b := range 3 {
		n, more := s.read(block)
		if n != BlockFrames || !more {
			t.Fatalf("block %d of the audio already delivered was refused (%d, %v); the"+
				" tail of every track that ends on its own goes with it", b, n, more)
		}
		if block[0] != 5000 {
			t.Fatalf("block %d came out silent after the stream was ended", b)
		}
	}
	if _, more := s.read(block); more {
		t.Error("a stream with nothing left to play still asks for blocks, so the writer" +
			" feeds silence and the amp never reaches standby")
	}
	if !s.Spent() {
		t.Error("a stream that played itself out does not report itself spent, so the" +
			" next track is written into a source the mixer has already dropped")
	}
}

func TestAFinishedStreamTakesNoMoreAudio(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	s.Finish()
	if err := s.Write(now.Add(time.Second), level(BlockFrames, 100)); err == nil {
		t.Error("audio was taken after the server said the stream had ended, so it plays" +
			" under whatever starts next")
	}
}

func TestAStreamThatCarriesOnKeepsItsMappingAndTakesAudioAgain(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	origin := s.origin
	s.Finish()
	s.Resume()

	if err := s.Write(now.Add(50*time.Millisecond), level(BlockFrames, 6000)); err != nil {
		t.Fatalf("Write after the stream carried on: %v", err)
	}
	if !s.origin.Equal(origin) {
		t.Error("carrying on moved the mapping, which is the 150 ms of silence that" +
			" opening a fresh stream costs")
	}
	block := make([]int16, BlockFrames)
	for range 6 {
		s.read(block)
	}
	if s.placed != BlockFrames {
		t.Errorf("%d frames were placed after the stream carried on, want %d",
			s.placed, BlockFrames)
	}
}

func TestAStreamHoldingAudioThatNeverComesDueGivesUp(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now.Add(streamAhead-time.Second), level(BlockFrames, 100)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Finish()
	s.doneAt = time.Now().Add(-drainWait)

	if _, more := s.read(make([]int16, BlockFrames)); more {
		t.Errorf("a finished stream holding audio due in %s still asks for blocks; it"+
			" would feed silence and hold the amp awake for that long",
			streamAhead-time.Second)
	}
}

func TestAudioIsQueuedByWhenItIsDueRatherThanWhenItArrived(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now.Add(10*time.Second), level(BlockFrames, 4000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write(now, level(BlockFrames, 6000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	s.read(block)
	if block[0] != 6000 {
		t.Errorf("the first sample is %d, want the chunk that was due; one chunk stamped"+
			" far ahead of the rest sat at the head of the queue and held everything"+
			" behind it, which is silence with nothing counted late and nothing logged",
			block[0])
	}
}

func TestAPartlyPlayedChunkKeepsItsPlaceInTheQueue(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, ramp(0, 2*BlockFrames)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockFrames)
	s.read(block)

	if err := s.Write(now.Add(-5*time.Millisecond), level(2*BlockFrames, 9000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.read(block)
	if want := int16(BlockFrames); block[0] != want {
		t.Errorf("the sample after a half-played chunk is %d, want %d: audio inserted"+
			" ahead of a chunk the mixer is partway through restarts it mid-sound",
			block[0], want)
	}
}

func TestAStreamThatOutlivesTheDurationTypeStillReportsHonestly(t *testing.T) {
	if got := frameTime(1 << 40); got <= 0 {
		t.Errorf("%d frames came back as %s; the counters a long stream accumulates"+
			" overflow the multiply and the closing line reports a negative duration",
			int64(1)<<40, got)
	}
	if got := frameTime(-3000); got != -62500*time.Microsecond {
		t.Errorf("-3000 frames came back as %s, want -62.5ms", got)
	}
	if got := frameTime(BlockFrames); got != 10*time.Millisecond {
		t.Errorf("a block came back as %s, want 10ms", got)
	}
}

func TestAStreamThatHasPlayedOutRefusesToCarryOn(t *testing.T) {
	s := quiet()
	anchorAt(s, time.Now())
	s.Finish()
	s.read(make([]int16, BlockFrames))
	if !s.Spent() {
		t.Fatal("the stream did not retire itself with an empty queue")
	}
	if s.Resume() {
		t.Error("a stream the mixer has already dropped said it could carry on. The" +
			" caller then keeps a dead handle, every write is refused, and the whole" +
			" next track is silent until a stream/start replaces it")
	}
}

func TestAStreamHoldsABoundedNumberOfChunksAndNotOnlyBoundedAudio(t *testing.T) {
	s := quiet()
	due := time.Now().Add(time.Second)
	one := level(1, 1000)
	for i := range streamChunks {
		if err := s.Write(due.Add(-time.Duration(i)*time.Microsecond), one); err != nil {
			t.Fatalf("chunk %d of %d was refused, and the queue holds a whole %s: %v",
				i, streamChunks, frameTime(streamHold), err)
		}
	}
	if err := s.Write(due.Add(-time.Duration(streamChunks)*time.Microsecond), one); err == nil {
		t.Fatalf("a %d-th chunk was taken, so an ordered insert over an unbounded queue"+
			" costs a server nothing while it holds the lock the mixer reads under",
			streamChunks+1)
	}
	s.mu.Lock()
	held := s.held
	s.mu.Unlock()
	if held >= streamHold {
		t.Fatalf("the queue held %s of audio, which is the frame ceiling of %s doing the"+
			" refusing rather than the chunk ceiling", frameTime(held), frameTime(streamHold))
	}
}

func TestAudioIsHeldWithTheMonotonicReadingItArrivedWith(t *testing.T) {
	s := quiet()
	at := time.Now().Add(time.Second)
	if err := s.Write(at, level(1, 1000)); err != nil {
		t.Fatalf("the stream would not take the audio: %v", err)
	}
	s.mu.Lock()
	held := s.queue[0].at
	s.mu.Unlock()
	if held != at {
		t.Fatalf("the queue holds %v where %v arrived, so the mapping subtracts a wall"+
			" reading frozen at daemon start from one read live, and a step of the wall"+
			" clock moves every frame this stream places", held, at)
	}
}

func TestAChunkDueFurtherOffThanTheFrameCountFitsLeavesTheWriterRunning(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now.Add(100*time.Millisecond), level(1200, 1000)); err != nil {
		t.Fatalf("the stream would not take the audio: %v", err)
	}
	s.mu.Lock()
	s.origin = s.origin.Add(-13 * time.Hour)
	s.mu.Unlock()

	block := make([]int16, BlockFrames)
	ran := make(chan struct{})
	go func() {
		s.read(block)
		close(ran)
	}()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatalf("read never returned for a chunk %s out, so a gap wider than a 32-bit"+
			" frame count spins inside the lock the mixer and the chime both wait on,"+
			" and this dot is silent until it reboots", 13*time.Hour)
	}
	if audio, silence := s.Placed(); audio != 0 || silence != int64(len(block)) {
		t.Fatalf("the block placed %d frames of audio and %d of silence, want %d silent:"+
			" a chunk that far out is a gap rather than something to play",
			audio, silence, len(block))
	}
}

func TestAStreamThatWasNeverPlacedSaysWhatItThrewAway(t *testing.T) {
	var said []string
	s := &Stream{say: func(format string, args ...any) {
		said = append(said, fmt.Sprintf(format, args...))
	}}
	if err := s.Write(time.Now().Add(time.Second), level(1200, 1000)); err != nil {
		t.Fatalf("the stream would not take the audio: %v", err)
	}
	s.Close()
	if len(said) != 1 {
		t.Fatalf("closing said %d things, want 1: %v", len(said), said)
	}
	if !strings.Contains(said[0], frameTime(1200).String()) {
		t.Errorf("closing said %q, which never names the %s it was still holding, so a"+
			" stream that was never placed reports nothing it lost", said[0],
			frameTime(1200))
	}
}
