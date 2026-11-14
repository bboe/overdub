package audio

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func clipRamp(frames int) []int16 {
	pcm := make([]int16, frames*ChimeChannels)
	for i := range frames {
		pcm[i*2], pcm[i*2+1] = int16(i), int16(-i)
	}
	return pcm
}

func drain(f *Feed) ([]int16, bool) {
	var out []int16
	block := make([]int16, BlockSamples)
	for range 10000 {
		n, more := f.read(block)
		out = append(out, block[:n]...)
		if !more {
			return out, true
		}
		if n == 0 {
			return out, false
		}
	}
	return out, false
}

func TestAClipAtThePlayersRatePlaysEverySampleThenEnds(t *testing.T) {
	var told atomic.Int32
	done := make(chan struct{})
	f := newFeed(ChimeRate, func() int { return ChimeRate }, func() { told.Add(1); close(done) })
	in := clipRamp(ChimeRate / 2)
	if err := f.Write(in); err != nil {
		t.Fatal(err)
	}
	f.Finish()
	out, ended := drain(f)
	if !ended {
		t.Fatal("a finished clip never said it was done")
	}
	if len(out) != len(in) {
		t.Fatalf("played %d samples of %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("sample %d is %d, want %d: a clip at the player's rate is not touched",
				i, out[i], in[i])
		}
	}
	<-done
	block := make([]int16, BlockSamples)
	if _, more := f.read(block); more {
		t.Error("a drained clip asked to stay in the mixer")
	}
	time.Sleep(10 * time.Millisecond)
	if got := told.Load(); got != 1 {
		t.Errorf("done was called %d times, want 1", got)
	}
}

func TestAClipWaitsForItsFirstBufferBeforeSounding(t *testing.T) {
	f := newFeed(ChimeRate, func() int { return ChimeRate }, nil)
	short := clipRamp(int(frameCount(ChimeRate, feedPrebuild)) - 1)
	if err := f.Write(short); err != nil {
		t.Fatal(err)
	}
	block := make([]int16, BlockSamples)
	if n, more := f.read(block); n != 0 || !more {
		t.Fatalf("read %d samples (more=%v) with less than %v queued, want silence and"+
			" more: a slow fetch would otherwise stutter from its first block",
			n, more, feedPrebuild)
	}
	if err := f.Write(clipRamp(1)); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.read(block); n == 0 {
		t.Error("the clip stayed silent once its first buffer was in")
	}
}

func TestAClipShorterThanTheFirstBufferStillPlays(t *testing.T) {
	f := newFeed(ChimeRate, func() int { return ChimeRate }, nil)
	if err := f.Write(clipRamp(100)); err != nil {
		t.Fatal(err)
	}
	f.Finish()
	out, ended := drain(f)
	if !ended || len(out) != 100*ChimeChannels {
		t.Errorf("played %d samples (ended=%v), want 200 and an end", len(out), ended)
	}
}

func TestAClipIsResampledToThePlayersRate(t *testing.T) {
	f := newFeed(ChimeRate, func() int { return BluetoothRate }, nil)
	in := clipRamp(ChimeRate)
	if err := f.Write(in); err != nil {
		t.Fatal(err)
	}
	f.Finish()
	out, ended := drain(f)
	if !ended {
		t.Fatal("the resampled clip never ended")
	}
	frames := len(out) / ChimeChannels
	if frames < BluetoothRate-2 || frames > BluetoothRate {
		t.Fatalf("a second at %d Hz became %d frames at %d Hz, want about %d",
			ChimeRate, frames, BluetoothRate, BluetoothRate)
	}
	for i := 0; float64(i)*ChimeRate/BluetoothRate < 32000; i += 997 {
		want := float64(i) * ChimeRate / BluetoothRate
		if got := float64(out[i*2]); got < want-1 || got > want+1 {
			t.Fatalf("frame %d is %v, want about %.1f: the ramp is interpolated", i, got, want)
		}
		if out[i*2+1] != -out[i*2] {
			t.Fatalf("frame %d: the channels came apart, %d and %d", i, out[i*2], out[i*2+1])
		}
	}
}

func TestAFullQueueHoldsTheWriterUntilThereIsRoom(t *testing.T) {
	f := newFeed(ChimeRate, func() int { return ChimeRate }, nil)
	full := clipRamp(int(frameCount(ChimeRate, feedQueue)))
	if err := f.Write(full); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() { wrote <- f.Write(clipRamp(10)) }()
	select {
	case err := <-wrote:
		t.Fatalf("a write into a full queue returned at once (%v): a fast fetch would"+
			" buffer the whole clip in memory", err)
	case <-time.After(50 * time.Millisecond):
	}
	block := make([]int16, BlockSamples)
	f.read(block)
	if err := <-wrote; err != nil {
		t.Errorf("the write after room was made failed: %v", err)
	}
}

func TestStoppingAClipFreesItsWriterAndEndsIt(t *testing.T) {
	done := make(chan struct{})
	f := newFeed(ChimeRate, func() int { return ChimeRate }, func() { close(done) })
	if err := f.Write(clipRamp(int(frameCount(ChimeRate, feedQueue)))); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() { wrote <- f.Write(clipRamp(10)) }()
	time.Sleep(20 * time.Millisecond)
	f.Stop()
	if err := <-wrote; !errors.Is(err, ErrFeedStopped) {
		t.Errorf("a writer held on a full queue got %v when the clip stopped, want"+
			" ErrFeedStopped", err)
	}
	block := make([]int16, BlockSamples)
	if n, more := f.read(block); n != 0 || more {
		t.Errorf("a stopped clip read %d samples (more=%v), want none and out", n, more)
	}
	<-done
}

func TestARateThatChangesMidClipReadsNothingPastTheQueue(t *testing.T) {
	rate := BluetoothRate
	f := newFeed(ChimeRate, func() int { return rate }, nil)
	if err := f.Write(clipRamp(ChimeRate)); err != nil {
		t.Fatal(err)
	}
	f.Finish()
	block := make([]int16, BlockSamples)
	f.read(block)
	rate = ChimeRate
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the clip panicked once a 48 kHz stream set the player back: %v; the"+
				" writer goroutine dies with it, and the daemon with that", r)
		}
	}()
	if _, ended := drain(f); !ended {
		t.Error("the clip never ended after the rate changed")
	}
}

func TestAClipDucksUnderAlexa(t *testing.T) {
	f := newFeed(ChimeRate, func() int { return ChimeRate }, nil)
	f.cutTo = func() float64 { return duckCut(20) }
	in := make([]int16, ChimeRate*ChimeChannels)
	for i := range in {
		in[i] = duckProbe
	}
	if err := f.Write(in); err != nil {
		t.Fatal(err)
	}
	f.Finish()
	out, ended := drain(f)
	if !ended {
		t.Fatal("the clip never ended")
	}
	want := attenuate(duckProbe, 1-duckCut(20))
	if out[0] <= want {
		t.Errorf("the first sample was %d, want above %d: the duck eases in", out[0], want)
	}
	if out[len(out)-1] != want {
		t.Errorf("the last sample was %d, want %d: a clip plays over alexa at full level",
			out[len(out)-1], want)
	}
}
