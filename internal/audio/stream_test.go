package audio

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func quiet() *Stream { return &Stream{rate: ChimeRate, say: func(string, ...any) {}} }

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
	block := make([]int16, frames*ChimeChannels)
	for i := range block {
		block[i] = at
	}
	buf := make([]byte, frames*frameBytes)
	encode(block, buf)
	return buf
}

func ramp(from, frames int) []byte {
	block := make([]int16, frames*ChimeChannels)
	for i := range block {
		block[i] = int16((from + i/ChimeChannels) % 30000)
	}
	buf := make([]byte, frames*frameBytes)
	encode(block, buf)
	return buf
}

func left(block []int16) []int16 {
	out := make([]int16, len(block)/ChimeChannels)
	for i := range out {
		out[i] = block[i*ChimeChannels]
	}
	return out
}

func TestAudioArrivingBeforeTheMappingWaitsForItRatherThanBeingSpent(t *testing.T) {
	s := quiet()
	now := time.Now()
	if err := s.Write(now.Add(200*time.Millisecond), level(BlockFrames, 5000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockSamples)
	for b := range anchorTake {
		if n, more := s.read(block); n != BlockSamples || !more {
			t.Fatalf("read returned %d, %v; an open stream keeps the writer fed", n, more)
		}
		for i, got := range left(block) {
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
	block := make([]int16, BlockSamples)
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
	block := make([]int16, BlockSamples)
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
	block := make([]int16, BlockSamples)
	for b := range 2 {
		s.read(block)
		for i, got := range left(block) {
			if got != 0 {
				t.Fatalf("block %d sample %d is %d; audio due 25 ms out was played in the"+
					" first %d ms, which is that far ahead of the rest of the group",
					b, i, got, (b+1)*10)
			}
		}
	}
	s.read(block)
	for i, got := range left(block) {
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

func TestAStreamPlacesAndCountsInItsOwnRatesFrames(t *testing.T) {
	s := quiet()
	s.rate = BluetoothRate
	now := time.Now()
	s.first = now.Add(-anchorSettle)
	for range anchorTake {
		s.observe(Point{Ahead: BluetoothRate / 10, At: now})
	}
	if err := s.Write(now.Add(150*time.Millisecond), level(1000, 4000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockSamples)
	var played []int16
	for range 6 {
		s.read(block)
		played = append(played, left(block)...)
	}
	if got := slices.IndexFunc(played, func(v int16) bool { return v != 0 }); got != BluetoothRate/20 {
		t.Errorf("audio due 50 ms past the frame the player reaches next began at frame %d,"+
			" want %d: a %d Hz stream counted in another rate's frames drifts from the"+
			" group by the ratio", got, BluetoothRate/20, BluetoothRate)
	}
	if _, silence := s.Placed(); silence != 50*time.Millisecond {
		t.Errorf("the stream reported %s of silence ahead of the chunk, want 50ms", silence)
	}
}

func TestAStreamFollowsReadingsInItsOwnRatesFrames(t *testing.T) {
	s := quiet()
	s.rate = BluetoothRate
	now := time.Now()
	block := make([]int16, BlockSamples)
	s.first = now.Add(-anchorSettle)
	for range anchorTake {
		s.observe(Point{At: now.Add(frameTime(BluetoothRate, s.index))})
		s.read(block)
	}
	if !s.anchored || !s.origin.Equal(now) {
		t.Fatalf("readings a block apart anchored the stream at %s from the truth; each"+
			" is taken back to the first by the frames between them, at the stream's"+
			" own rate", s.origin.Sub(now))
	}
	for b := range 300 {
		s.read(block)
		if b%observeEvery == 0 {
			s.observe(Point{At: now.Add(frameTime(BluetoothRate, s.index))})
		}
	}
	if s.slips != 0 {
		t.Errorf("readings that agree with a %d Hz stream placed it again %d times in"+
			" 3 seconds: a slip counted in another rate's frames grows about 80 ms a"+
			" second", BluetoothRate, s.slips)
	}
}

func TestAChunkPastTheSnapIsPlacedWhereItIsDueAtItsOwnRate(t *testing.T) {
	s := quiet()
	s.rate = BluetoothRate
	now := time.Now()
	anchorAt(s, now)
	block := make([]int16, BlockSamples)
	if err := s.Write(now, level(BlockFrames, 1000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.read(block)
	const late = 2300
	if err := s.Write(now.Add(frameTime(BluetoothRate, BlockFrames+late)), level(BlockFrames, 4000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var played []int16
	for range 8 {
		s.read(block)
		played = append(played, left(block)...)
	}
	if got := slices.IndexFunc(played, func(v int16) bool { return v != 0 }); got < late-1 || got > late {
		t.Errorf("a chunk due %d frames on, past the %s snap at %d Hz, began at frame %d:"+
			" it was eased toward its place instead of put there", late, snapAbove,
			BluetoothRate, got)
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
	block := make([]int16, BlockSamples)
	for b := range 4 * chunk / BlockFrames {
		s.read(block)
		for i, got := range left(block) {
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
	block := make([]int16, BlockSamples)
	s.read(block)
	for i, got := range left(block) {
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
	block := make([]int16, BlockSamples)
	s.read(block)
	for i, got := range left(block) {
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
	block := make([]int16, BlockSamples)
	for b := range 3 {
		s.read(block)
		for i, got := range left(block) {
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
	for _, rate := range Rates() {
		s := quiet()
		s.rate = rate
		now := time.Now()
		for i := range 30 {
			if err := s.Write(now, level(rate, 100)); err != nil {
				t.Fatalf("at %d Hz, second %d of the 30 aiosendspin may send ahead: %v;"+
					" buffer_capacity counts bytes, so a FLAC stream through a quiet"+
					" passage reaches that cap", rate, i, err)
			}
		}
		if err := s.Write(now, level(1, 100)); err == nil {
			t.Errorf("at %d Hz a server can buffer past %s here, which is memory a peer"+
				" chooses the size of on a device with 512 MB", rate, streamHold)
		}
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
				" way to a frame index", when.Sub(now), frameTime(ChimeRate, 1<<50))
		}
	}
}

func TestAPartialFrameIsRefusedRatherThanPairedWithTheNextWrite(t *testing.T) {
	for _, n := range []int{3, 2, 6, 10} {
		s := quiet()
		if err := s.Write(time.Now(), make([]byte, n)); err == nil {
			t.Errorf("%d bytes were taken, which is not whole %d-byte frames, and pairs"+
				" every later sample with the wrong channel for as long as the stream runs",
				n, frameBytes)
		}
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
	block := make([]int16, BlockSamples)
	if n, more := s.read(block); n != BlockSamples || !more {
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
	if n, more := s.read(make([]int16, BlockSamples)); n != 0 || more {
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
	s := &Stream{rate: ChimeRate, say: func(_ string, args ...any) { lines = append(lines, args) }}
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, level(BlockFrames, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write(now.Add(-time.Second), level(240, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	block := make([]int16, BlockSamples)
	s.read(block)
	s.read(block)
	if err := s.Write(now.Add(time.Second), level(720, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Finish()
	s.Close()

	if len(lines) != 1 {
		t.Fatalf("a stream that ended said %d things; the only place the silence it"+
			" inserted and the audio it dropped are readable is this line", len(lines))
	}
	want := []any{frameTime(ChimeRate, BlockFrames), frameTime(ChimeRate, BlockFrames), frameTime(ChimeRate, 240),
		frameTime(ChimeRate, 720), frameTime(ChimeRate, 0), 0}
	for i, got := range lines[0] {
		if got != want[i] {
			t.Errorf("the line reports %v at position %d, want %v: one block of audio"+
				" played, one of silence after it, a chunk a second late dropped, a"+
				" chunk still buffered when the stream ended, and nothing eased",
				got, i, want[i])
		}
	}
}

func TestAStreamThatNeverAnchoredStillSaysWhatItThrewAway(t *testing.T) {
	var lines [][]any
	s := &Stream{rate: ChimeRate, say: func(_ string, args ...any) { lines = append(lines, args) }}
	if err := s.Write(time.Now().Add(time.Second), level(4800, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Finish()
	s.Close()

	if len(lines) != 1 {
		t.Fatalf("a stream that never anchored said %d things", len(lines))
	}
	if got := lines[0][1]; got != frameTime(ChimeRate, 4800) {
		t.Errorf("the line reports %v thrown away, want %v; ending the stream empties"+
			" the queue, so a report reading the queue afterwards says a whole stream"+
			" was worth nothing", got, frameTime(ChimeRate, 4800))
	}
}

func TestASmallSlipIsNotWorthSkippingTheAudioFor(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	origin := s.origin

	for range 200 {
		s.observe(Point{At: now.Add(anchorSlip / 2)})
	}
	if s.slips != 0 || !s.origin.Equal(origin) {
		t.Errorf("readings %s off moved the mapping; the instrument itself is only good to a"+
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
	if got := s.candidates[0].Sub(now); got != frameTime(ChimeRate, depth) {
		t.Errorf("the next frame was placed %s out, want the %s the player and the HAL"+
			" still hold between them", got, frameTime(ChimeRate, depth))
	}
}

func TestAPlayerThatWillNotSayWhereItIsSaysSoOnce(t *testing.T) {
	said := 0
	s := &Stream{rate: ChimeRate, say: func(string, ...any) { said++ }}
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
		block := make([]int16, BlockSamples)
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
	s := &Stream{rate: ChimeRate, say: func(_ string, args ...any) { said = append(said, args...) }}
	s.first = time.Now().Add(-anchorSettle)
	const depth = 6896
	for range anchorTake {
		s.observe(Point{Ahead: depth, At: time.Now()})
	}
	if len(said) != 1 {
		t.Fatalf("placing a stream said %d things, want the one line that makes the"+
			" declared required_lead_time_ms checkable on hardware", len(said))
	}
	if got := said[0]; got != frameTime(ChimeRate, depth).Round(time.Millisecond) {
		t.Errorf("the line reports %v, want the %s the player and the HAL hold between"+
			" a written frame and the speaker", got, frameTime(ChimeRate, depth))
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

func TestAFinishedStreamDropsWhatItStillHolds(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, level(3*BlockFrames, 5000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Finish()

	block := make([]int16, BlockSamples)
	if n, more := s.read(block); n != 0 || more {
		t.Fatalf("a finished stream delivered %d frames and asked for more (%v); a pause"+
			" then takes as long as the buffer to go quiet", n, more)
	}
	if !s.Spent() {
		t.Error("a stream that was ended does not report itself spent, so the" +
			" next track is written into a source the mixer has already dropped")
	}
	if s.held != 0 {
		t.Errorf("%d frames survived the end of the stream", s.held)
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
	block := make([]int16, BlockSamples)
	for range 6 {
		s.read(block)
	}
	if s.placed != BlockFrames {
		t.Errorf("%d frames were placed after the stream carried on, want %d",
			s.placed, BlockFrames)
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
	block := make([]int16, BlockSamples)
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
	block := make([]int16, BlockSamples)
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
	if got := frameTime(ChimeRate, 1<<40); got <= 0 {
		t.Errorf("%d frames came back as %s; the counters a long stream accumulates"+
			" overflow the multiply and the closing line reports a negative duration",
			int64(1)<<40, got)
	}
	if got := frameTime(ChimeRate, -3000); got != -62500*time.Microsecond {
		t.Errorf("-3000 frames came back as %s, want -62.5ms", got)
	}
	if got := frameTime(ChimeRate, BlockFrames); got != 10*time.Millisecond {
		t.Errorf("a block came back as %s, want 10ms", got)
	}
}

func TestAStreamThatHasPlayedOutRefusesToCarryOn(t *testing.T) {
	s := quiet()
	anchorAt(s, time.Now())
	s.Finish()
	s.read(make([]int16, BlockSamples))
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
				i, streamChunks, streamHold, err)
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
	if held >= frameCount(ChimeRate, streamHold) {
		t.Fatalf("the queue held %s of audio, which is the frame ceiling of %s doing the"+
			" refusing rather than the chunk ceiling", frameTime(ChimeRate, held), streamHold)
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

	block := make([]int16, BlockSamples)
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
	if audio, silence := s.Placed(); audio != 0 || silence != frameTime(ChimeRate, BlockFrames) {
		t.Fatalf("the block placed %s of audio and %s of silence, want %s silent:"+
			" a chunk that far out is a gap rather than something to play",
			audio, silence, frameTime(ChimeRate, BlockFrames))
	}
}

func TestAStreamThatWasNeverPlacedSaysWhatItThrewAway(t *testing.T) {
	var said []string
	s := &Stream{rate: ChimeRate, say: func(format string, args ...any) {
		said = append(said, fmt.Sprintf(format, args...))
	}}
	if err := s.Write(time.Now().Add(time.Second), level(1200, 1000)); err != nil {
		t.Fatalf("the stream would not take the audio: %v", err)
	}
	s.Close()
	if len(said) != 1 {
		t.Fatalf("closing said %d things, want 1: %v", len(said), said)
	}
	if !strings.Contains(said[0], frameTime(ChimeRate, 1200).String()) {
		t.Errorf("closing said %q, which never names the %s it was still holding, so a"+
			" stream that was never placed reports nothing it lost", said[0],
			frameTime(ChimeRate, 1200))
	}
}

func TestATimingErrorIsCorrectedOneFrameAtATime(t *testing.T) {
	for _, rate := range Rates() {
		s := quiet()
		s.rate = rate
		band := frameCount(rate, deadBand)
		for _, c := range []struct {
			err  int64
			want int64
		}{
			{band - 1, 0},
			{-(band - 1), 0},
			{band, 1},
			{-band, -1},
			{int64(rate), 1},
			{-int64(rate), -1},
		} {
			if got := s.ease(c.err); got != c.want {
				t.Errorf("at %d Hz an error of %s was corrected by %d frames, want %d:"+
					" chasing the whole error is what makes every clock revision audible",
					rate, frameTime(rate, c.err), got, c.want)
			}
		}
	}
}

func TestAnInsertedFrameRampsBetweenItsNeighbours(t *testing.T) {
	block := make([]int16, 3*ChimeChannels)
	blend(block, []int16{0, 800}, []int16{400, 0})
	for i, want := range [][]int16{{100, 600}, {200, 400}, {300, 200}} {
		if got := block[i*ChimeChannels : (i+1)*ChimeChannels]; !slices.Equal(got, want) {
			t.Errorf("inserted frame %d is %v, want %v: a repeated or silent frame is a"+
				" step in the waveform, which is what a pure tone clicks on, and each"+
				" channel ramps between its own neighbours", i, got, want)
		}
	}
}

func TestOneRevisionOfTheClockDoesNotMoveTheAudio(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	at := now
	block := make([]int16, BlockSamples)
	for i := range 8 {
		if i == 4 {
			at = at.Add(-3 * time.Millisecond)
		}
		if err := s.Write(at, level(BlockFrames, 3000)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		at = at.Add(frameTime(ChimeRate, BlockFrames))
		s.read(block)
	}
	if s.late > 1 {
		t.Errorf("a single 3ms revision cost %s of audio; one revision is the"+
			" instrument moving, not the audio being late", frameTime(ChimeRate, s.late))
	}
}

func TestAnEmptyChunkIsNotPlacedRatherThanIndexed(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	s.began = true
	s.smooth = 2 * frameCount(ChimeRate, deadBand) << smoothBits
	if err := s.Write(now.Add(2*time.Millisecond), nil); err != nil {
		t.Fatalf("Write of an empty chunk: %v", err)
	}
	block := make([]int16, BlockSamples)
	for range 10 {
		s.read(block)
	}
	if len(s.queue) != 0 {
		t.Errorf("the empty chunk is still queued, so it is still in front of whatever"+
			" arrives next: %d entries", len(s.queue))
	}
	if s.placed != 0 || s.eased != 0 {
		t.Errorf("a chunk carrying no audio placed %s and eased %s",
			frameTime(ChimeRate, s.placed), frameTime(ChimeRate, s.eased))
	}
}

func TestFramesEasedOntoTheClockAreNotCountedLate(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	at := now
	block := make([]int16, BlockSamples)
	for range 200 {
		if err := s.Write(at, level(BlockFrames, 3000)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		at = at.Add(frameTime(ChimeRate, BlockFrames) - 200*time.Microsecond)
		s.read(block)
	}
	if s.eased == 0 {
		t.Fatal("a stream tracking a drifting clock eased nothing, so this says nothing")
	}
	if s.late != 0 {
		t.Errorf("%s was counted late, and it was eased on purpose: the count is what"+
			" separates a starved stream from a healthy one", frameTime(ChimeRate, s.late))
	}
}

func TestAReanchorLeavesAPartPlayedChunkWhereItIs(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	at := now
	block := make([]int16, BlockSamples)
	for range 3 {
		if err := s.Write(at, level(2*BlockFrames, 3000)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		at = at.Add(frameTime(ChimeRate, 2*BlockFrames))
	}
	s.read(block)
	for range slipRuns {
		s.place(Point{At: time.Now().Add(-60 * time.Millisecond)})
	}
	if filled, _, _ := s.fill(block); filled == 0 {
		t.Error("a whole block after a re-anchor carried no audio: a chunk the mixer is" +
			" partway through is placed against its own frame 0, so re-deciding it" +
			" moves it by everything already played")
	}
}

func TestFramesEasedInOrOutAreNotCountedAsSilence(t *testing.T) {
	for _, drift := range []time.Duration{-40 * time.Microsecond, 40 * time.Microsecond} {
		s := quiet()
		now := time.Now()
		anchorAt(s, now)
		at := now
		block := make([]int16, BlockSamples)
		for range 3 {
			if err := s.Write(at, level(BlockFrames, 3000)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			at = at.Add(frameTime(ChimeRate, BlockFrames))
		}
		for range 300 {
			if err := s.Write(at, level(BlockFrames, 3000)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			at = at.Add(frameTime(ChimeRate, BlockFrames) + drift)
			s.read(block)
		}
		if s.eased == 0 {
			t.Fatalf("drifting %v: nothing was eased, so this says nothing", drift)
		}
		if s.silence < 0 {
			t.Errorf("drifting %v: silence is %s; a trimmed frame never reached a block,"+
				" and a blended frame is counted once, so neither stands in for silence",
				drift, frameTime(ChimeRate, s.silence))
		}
	}
}

func TestABlockThatRanOutLeavesNothingToRampFrom(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	if err := s.Write(now, level(BlockFrames/2, 3000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.read(make([]int16, BlockSamples))
	if s.lastOut != [ChimeChannels]int16{} {
		t.Errorf("the stream ramps from %v after a block that ended in silence; the"+
			" frames actually handed over were zeros, so an inserted frame would step"+
			" half way out of nothing", s.lastOut)
	}
}

func TestAShortChunkIsStillCorrected(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	at := now
	short := BlockFrames / 8
	block := make([]int16, BlockSamples)
	for range 2000 {
		if err := s.Write(at, level(short, 3000)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		at = at.Add(frameTime(ChimeRate, int64(short)) - 40*time.Microsecond)
		if len(s.queue) >= 8 {
			s.read(block)
		}
	}
	if s.eased == 0 {
		t.Error("a server sending short chunks got no correction at all, so the error" +
			" runs until it passes snapAbove and the stream jumps 50 ms")
	}
}

func anchorBursty(s *Stream, when time.Time) {
	s.first = time.Now().Add(-anchorSettle)
	for range anchorTake {
		s.observe(Point{At: when, Bursty: true})
	}
	if !s.anchored {
		panic("the stream did not learn where the player had reached")
	}
}

func TestBurstyReadingsAreFollowedByTheirAverage(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorBursty(s, now)

	const truth = 30 * time.Millisecond
	for i := range 20 * burstyOver {
		scatter := time.Duration(i%17-8) * 10 * time.Millisecond
		s.observe(Point{At: now.Add(truth + scatter), Bursty: true})
	}
	if s.slips != 0 {
		t.Errorf("readings scattered %s either way were followed with %d skips; measured"+
			" over Bluetooth, one reading lands anywhere in about 100 ms", 80*time.Millisecond,
			s.slips)
	}
	const within = 10 * time.Millisecond
	if off := s.origin.Sub(now) - truth; off > within || off < -within {
		t.Errorf("the mapping settled %s from where the readings average, want within %s",
			off, within)
	}
}

func TestTheFirstBurstySettleJumpsToTheMeanOfItsReadings(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorBursty(s, now)
	origin := s.origin

	const mean = 20 * time.Millisecond
	const settleAfter = 20
	reading := func(i int) Point {
		return Point{At: now.Add(mean + time.Duration(i%5-2)*10*time.Millisecond), Bursty: true}
	}
	for i := range settleAfter - 1 {
		s.observe(reading(i))
	}
	if !s.origin.Equal(origin) || s.jump {
		t.Fatalf("the mapping moved before %d readings were in; one burst's reading is off by"+
			" up to 100 ms over Bluetooth", settleAfter)
	}
	s.observe(reading(settleAfter - 1))
	if got := s.origin.Sub(origin); got != mean {
		t.Errorf("the first settle moved the mapping %s, want the %s its readings average", got,
			mean)
	}
	if !s.jump {
		t.Error("the first settle was left to easing, which moves about 1 ms a second, so a" +
			" stream starts out of step with its group for up to a minute")
	}
	if s.slips != 0 {
		t.Errorf("the first settle was counted as %d re-placements", s.slips)
	}
}

func TestASteadyBurstyOffsetAfterTheSettleIsFollowedWithoutPlacingAgain(t *testing.T) {
	for _, offset := range []time.Duration{2 * anchorSlip, -2 * anchorSlip} {
		s := quiet()
		now := time.Now()
		anchorBursty(s, now)
		for range burstyFirst {
			s.observe(Point{At: now, Bursty: true})
		}
		s.jump = false

		peak := time.Duration(0)
		for range 20 * burstyOver {
			s.observe(Point{At: now.Add(offset), Bursty: true})
			moved := s.origin.Sub(now)
			if offset < 0 {
				moved = -moved
			}
			peak = max(peak, moved)
		}
		if peak > offset.Abs()+10*time.Millisecond {
			t.Errorf("following a steady %s offset overshot by %s; each move has to start the"+
				" average again, or the next reading moves it by the same amount twice", offset,
				peak-offset.Abs())
		}
		if s.slips != 0 || s.jump {
			t.Errorf("a %s offset reached after the settle placed the stream again (%d slips,"+
				" jump %v); the average follows an offset that small", offset, s.slips, s.jump)
		}
		const within = 10 * time.Millisecond
		if off := s.origin.Sub(now) - offset; off > within || off < -within {
			t.Errorf("the mapping stopped %s short of a steady %s offset", off, offset)
		}
	}
}

func TestAJumpLongerThanAChunkCutsAllOfIt(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorBursty(s, now)
	const chunk = 1200
	for k := range 12 {
		at := now.Add(frameTime(ChimeRate, int64(k*chunk)))
		if err := s.Write(at, level(chunk, int16(100*(k+1)))); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	block := make([]int16, BlockSamples)
	s.read(block)

	const cut = 40 * time.Millisecond
	for range burstyFirst {
		s.observe(Point{At: now.Add(frameTime(ChimeRate, BlockFrames) + cut), Bursty: true})
	}
	var out []int16
	for range 25 {
		s.read(block)
		out = append(out, left(block)...)
	}
	got := BlockFrames + slices.Index(out, 1000)
	if want := 9*chunk - 1920; got != want {
		t.Errorf("after a %s jump the 10th chunk started at frame %d, want %d; the jump was"+
			" spent on a chunk it dropped whole, and the rest was left to easing", cut, got,
			want)
	}
}

func TestAJumpPlacesTheNextChunkWhereItsTimestampSays(t *testing.T) {
	for _, jump := range []bool{true, false} {
		s := quiet()
		now := time.Now()
		anchorAt(s, now)
		if err := s.Write(now, level(BlockFrames, 1000)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		block := make([]int16, BlockSamples)
		s.read(block)

		s.jump = jump
		if err := s.Write(now.Add(frameTime(ChimeRate, BlockFrames+960)), level(BlockFrames, 2000)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		var out []int16
		for range 4 {
			s.read(block)
			out = append(out, left(block)...)
		}
		first := slices.IndexFunc(out, func(v int16) bool { return v != 0 })
		if jump && (first != 960 || s.jump) {
			t.Errorf("after a jump the chunk started %d frames on, want the 960 its timestamp"+
				" names, with the jump spent (still set: %v)", first, s.jump)
		}
		if !jump && first == 960 {
			t.Error("without a jump a 20 ms gap was cut in whole rather than eased")
		}
	}
}

func TestAChangeOfOutputPlacesTheStreamAfresh(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)

	later := now.Add(200 * time.Millisecond)
	s.observe(Point{At: later, Bursty: true})
	if s.anchored {
		t.Fatal("the output changed under the stream and the old mapping was kept; the" +
			" depth moves by about 200 ms, which the average would creep towards for" +
			" seconds")
	}
	for range anchorTake - 1 {
		s.observe(Point{At: later, Bursty: true})
	}
	if !s.anchored || !s.origin.Equal(later) {
		t.Errorf("after the change the stream was placed at %s, want %s", s.origin.Sub(now),
			later.Sub(now))
	}
}

func TestScatteredBurstyReadingsAroundTheMappingLeaveItAlone(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorBursty(s, now)
	for range burstyFirst {
		s.observe(Point{At: now, Bursty: true})
	}
	moves, last := 0, s.origin
	for i := range 20 * burstyOver {
		scatter := time.Duration(i%17-8) * 10 * time.Millisecond
		s.observe(Point{At: now.Add(scatter), Bursty: true})
		if !s.origin.Equal(last) {
			moves, last = moves+1, s.origin
		}
	}
	if moves > 5 {
		t.Errorf("readings scattered 80 ms either way of the mapping moved it %d times; every"+
			" move is a correction the listener pays for, chasing one burst's phase", moves)
	}
}

func TestAReallyLargeBurstySlipIsPlacedAgainAtOnce(t *testing.T) {
	for _, slip := range []time.Duration{time.Second, -time.Second} {
		s := quiet()
		now := time.Now()
		anchorBursty(s, now)
		for range burstyFirst {
			s.observe(Point{At: now.Add(10 * time.Millisecond), Bursty: true})
		}
		s.jump = false

		settled := s.origin
		far := settled.Add(slip)
		for _, at := range []time.Time{far, far, settled, far, far} {
			s.observe(Point{At: at, Bursty: true})
		}
		if s.slips != 0 {
			t.Fatalf("%s slips broken by a reading in step moved the mapping; 3 in a row are"+
				" the rule, as on the speaker", slip)
		}
		s.observe(Point{At: far, Bursty: true})
		if s.slips != 1 || !s.origin.Equal(far) || !s.jump {
			t.Errorf("a %s slip held for 3 readings left the mapping %s out with %d slips"+
				" (jump %v); following it by the average takes over 20 seconds", slip,
				far.Sub(s.origin), s.slips, s.jump)
		}
		const off = 20 * time.Millisecond
		for range burstyFirst - 1 {
			s.observe(Point{At: far.Add(off), Bursty: true})
		}
		s.jump = false
		s.observe(Point{At: far.Add(off), Bursty: true})
		if !s.jump || s.origin.Sub(far) != off {
			t.Errorf("after it was placed again the stream settled %s on (jump %v), want the"+
				" %s its new readings average", s.origin.Sub(far), s.jump, off)
		}
	}
}

func TestAChangeBackToTheSpeakerPlacesTheStreamAfresh(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorBursty(s, now)

	later := now.Add(-300 * time.Millisecond)
	s.observe(Point{At: later})
	if s.anchored {
		t.Fatal("the stream kept its Bluetooth mapping after the output went back to the" +
			" speaker")
	}
	for range anchorTake - 1 {
		s.observe(Point{At: later})
	}
	if !s.anchored || !s.origin.Equal(later) || s.bursty {
		t.Errorf("back on the speaker the stream was placed at %s (bursty %v), want %s",
			s.origin.Sub(now), s.bursty, later.Sub(now))
	}
}

func TestTheSettleRunsAgainWhenBluetoothComesBack(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorBursty(s, now)
	for range burstyFirst {
		s.observe(Point{At: now.Add(10 * time.Millisecond), Bursty: true})
	}
	for range anchorTake {
		s.observe(Point{At: now})
	}
	for range anchorTake {
		s.observe(Point{At: now, Bursty: true})
	}
	s.jump = false
	const off = 25 * time.Millisecond
	for range burstyFirst {
		s.observe(Point{At: now.Add(off), Bursty: true})
	}
	if !s.jump || s.origin.Sub(now) != off {
		t.Errorf("the second Bluetooth anchor was not settled: jump %v, mapping at %s, want %s",
			s.jump, s.origin.Sub(now), off)
	}
}

func TestAChangeOfOutputWhileAnchoringStartsTheReadingsOver(t *testing.T) {
	s := quiet()
	s.first = time.Now().Add(-anchorSettle)
	now := time.Now()
	for range 3 {
		s.observe(Point{At: now.Add(330 * time.Millisecond), Bursty: true})
	}
	for range anchorTake {
		s.observe(Point{At: now})
	}
	if !s.anchored || !s.origin.Equal(now) || s.slips != 0 {
		t.Errorf("readings from 2 outputs anchored the stream at %s with %d re-placements,"+
			" want the speaker's own %s with none", s.origin.Sub(now), s.slips,
			time.Duration(0))
	}
}

func TestARunOfSlipsDoesNotCarryAcrossAnOutputChange(t *testing.T) {
	s := quiet()
	now := time.Now()
	anchorAt(s, now)
	for range slipRuns - 1 {
		s.observe(Point{At: now.Add(2 * anchorSlip)})
	}
	for range anchorTake {
		s.observe(Point{At: now, Bursty: true})
	}
	for range anchorTake {
		s.observe(Point{At: now})
	}
	s.observe(Point{At: now.Add(2 * anchorSlip)})
	if s.slips != 0 {
		t.Error("one reading after 2 output changes re-placed the stream, because the run" +
			" of slips from before them was still counted")
	}
}

func stereoRamp(from, frames int) []byte {
	block := make([]int16, frames*ChimeChannels)
	for i := range frames {
		v := int16((from+i)%30000 + 1)
		block[i*ChimeChannels], block[i*ChimeChannels+1] = v, -v
	}
	buf := make([]byte, frames*frameBytes)
	encode(block, buf)
	return buf
}

func TestEveryFrameKeepsItsChannelsInOrder(t *testing.T) {
	for _, c := range []struct {
		what   string
		chunk  int
		drift  time.Duration
		chunks int
		eases  bool
	}{
		{"chunks that do not divide into blocks", 1200, 0, 4, false},
		{"a server clock running fast, eased by trimming", BlockFrames, -200 * time.Microsecond, 300, true},
		{"a server clock running slow, eased by blending", BlockFrames, 200 * time.Microsecond, 300, true},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := quiet()
			now := time.Now()
			anchorAt(s, now)
			at := now
			block := make([]int16, BlockSamples)
			written, read := 0, 0
			for k := range c.chunks {
				if err := s.Write(at, stereoRamp(k*c.chunk, c.chunk)); err != nil {
					t.Fatalf("Write %d: %v", k, err)
				}
				at = at.Add(frameTime(ChimeRate, int64(c.chunk)) + c.drift)
				for written += c.chunk; read+BlockFrames <= written; read += BlockFrames {
					s.read(block)
					for i := range BlockFrames {
						l, r := block[i*ChimeChannels], block[i*ChimeChannels+1]
						if r != -l {
							t.Fatalf("frame %d is (%d, %d); every frame written was (v, -v),"+
								" so a channel moved against the other", read+i, l, r)
						}
					}
				}
			}
			if c.eases && s.eased == 0 {
				t.Fatal("nothing was eased, so the edits this checks never ran")
			}
			if s.placed == 0 {
				t.Fatal("no audio was placed, so there was nothing to check")
			}
		})
	}
}

func slowRamp(from, frames int) []byte {
	block := make([]int16, frames*ChimeChannels)
	for i := range frames {
		v := int16((from+i)/32 + 1)
		block[i*ChimeChannels], block[i*ChimeChannels+1] = v, -v
	}
	buf := make([]byte, frames*frameBytes)
	encode(block, buf)
	return buf
}

func TestEasingNeverStepsEitherChannel(t *testing.T) {
	for _, c := range []struct {
		chunk int
		drift time.Duration
	}{
		{BlockFrames, 40 * time.Microsecond},
		{BlockFrames, -40 * time.Microsecond},
		{1200, 40 * time.Microsecond},
		{1200, -40 * time.Microsecond},
		{1000, 40 * time.Microsecond},
		{1000, -40 * time.Microsecond},
	} {
		s := quiet()
		now := time.Now()
		anchorAt(s, now)
		at := now
		block := make([]int16, BlockSamples)
		written, read := 0, 0
		prev, started := int16(0), false
		for k := range 600 {
			if err := s.Write(at, slowRamp(k*c.chunk, c.chunk)); err != nil {
				t.Fatalf("Write %d: %v", k, err)
			}
			at = at.Add(frameTime(ChimeRate, int64(c.chunk)) + c.drift)
			for written += c.chunk; read+BlockFrames+ChimeRate/2 <= written; read += BlockFrames {
				s.read(block)
				for i := range BlockFrames {
					l, r := block[i*ChimeChannels], block[i*ChimeChannels+1]
					if !started && l == 0 {
						continue
					}
					started = true
					if r != -l || l == 0 || l < prev {
						t.Fatalf("%d-frame chunks drifting %v: frame %d is (%d, %d) after %d;"+
							" each channel only rises, so an inserted or trimmed frame stepped"+
							" one of them or left a hole", c.chunk, c.drift, read+i, l, r, prev)
					}
					prev = l
				}
			}
		}
		if s.eased == 0 {
			t.Errorf("%d-frame chunks drifting %v: nothing was eased, so no edit was checked",
				c.chunk, c.drift)
		}
	}
}

func TestATrimCutsOneFrameAndSmoothsTheCut(t *testing.T) {
	for _, chunk := range []int{BlockFrames, 1200, 1000} {
		s := quiet()
		now := time.Now()
		anchorAt(s, now)
		at := now
		block := make([]int16, BlockSamples)
		written, read, halfway := 0, 0, 0
		counts := map[int16]int{}
		var first, last int16
		for k := range 600 {
			if err := s.Write(at, level(chunk, int16(50*(k+1)))); err != nil {
				t.Fatalf("Write %d: %v", k, err)
			}
			at = at.Add(frameTime(ChimeRate, int64(chunk)) - 40*time.Microsecond)
			for written += chunk; read+BlockFrames+ChimeRate/2 <= written; read += BlockFrames {
				s.read(block)
				for i := range BlockFrames {
					l, r := block[i*ChimeChannels], block[i*ChimeChannels+1]
					switch {
					case r != l:
						t.Fatalf("%d-frame chunks: frame %d is (%d, %d); every frame written"+
							" was the same in both channels", chunk, read+i, l, r)
					case l == 0:
					case l%50 != 0:
						halfway++
					default:
						if first == 0 {
							first = l
						}
						counts[l]++
						last = l
					}
				}
			}
		}
		if s.eased == 0 {
			t.Fatalf("%d-frame chunks: nothing was eased, so no edit was checked", chunk)
		}
		if int64(halfway) != s.eased {
			t.Errorf("%d-frame chunks: %d frames sit halfway across a cut, for %d edits;"+
				" each edit smooths exactly one", chunk, halfway, s.eased)
		}
		for l, n := range counts {
			if l != first && l != last && n != chunk && n != chunk-2 {
				t.Errorf("%d-frame chunks: the chunk at level %d played %d frames; a chunk"+
					" plays whole, or loses the trimmed frame and the one smoothed after it",
					chunk, l, n)
			}
		}
	}
}
