package opus

import (
	"bytes"
	"os"
	"testing"
)

const (
	turnPackets = 139
	turnWakeEnd = 18080
)

func turn(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/turn.opus")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestACapturedTurnIsWholeWidebandMonoPackets(t *testing.T) {
	packets, rest := Split(turn(t))
	if len(packets) != turnPackets || len(rest) != 0 {
		t.Fatalf("got %d packets and %d bytes over, want %d and 0", len(packets), len(rest), turnPackets)
	}
	for i, p := range packets {
		config, stereo := p[0]>>3, p[0]&0x04 != 0
		if config != 9 || stereo {
			t.Fatalf("packet %d: TOC %#x is config %d, stereo %v; want 9 (SILK wideband, 20 ms), mono",
				i, p[0], config, stereo)
		}
	}
}

func TestSplitCarriesAPartialPacketIntoTheNextChunk(t *testing.T) {
	whole := turn(t)
	var got [][]byte
	var rest []byte
	for _, size := range []int{1, 79, 80, 81, 159, 4096} {
		for off := 0; off < len(whole); off += size {
			chunk := whole[off:min(off+size, len(whole))]
			var packets [][]byte
			packets, rest = Split(append(rest, chunk...))
			for _, p := range packets {
				got = append(got, bytes.Clone(p))
			}
			rest = bytes.Clone(rest)
		}
		if len(rest) != 0 || len(got) != turnPackets {
			t.Fatalf("chunks of %d: %d packets and %d bytes over", size, len(got), len(rest))
		}
		if !bytes.Equal(bytes.Join(got, nil), whole) {
			t.Fatalf("chunks of %d: the packets do not rejoin into the stream", size)
		}
		got = nil
	}
}

func TestSplitLeavesAShortStreamWhole(t *testing.T) {
	packets, rest := Split(make([]byte, PacketBytes-1))
	if len(packets) != 0 || len(rest) != PacketBytes-1 {
		t.Fatalf("got %d packets and %d bytes over", len(packets), len(rest))
	}
}

func numbered(from, n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(from + i)
	}
	return pcm
}

func TestSkipDropsTheWakeWordAcrossFrames(t *testing.T) {
	skip := NewSkip(turnWakeEnd)
	var kept []int16
	for from := 0; from < turnPackets*FrameSamples; from += FrameSamples {
		kept = append(kept, skip.Apply(numbered(from%30000, FrameSamples))...)
	}
	if want := turnPackets*FrameSamples - turnWakeEnd; len(kept) != want {
		t.Fatalf("kept %d samples, want %d", len(kept), want)
	}
	if kept[0] != turnWakeEnd%30000 {
		t.Fatalf("the first sample kept is %d, want %d", kept[0], turnWakeEnd%30000)
	}
}

func TestSkipOfNoSamplesKeepsEverything(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := NewSkip(n).Apply(numbered(0, FrameSamples)); len(got) != FrameSamples {
			t.Fatalf("NewSkip(%d) kept %d of %d samples", n, len(got), FrameSamples)
		}
	}
}

func TestSkipEndingOnAFrameBoundaryKeepsTheNextFrameWhole(t *testing.T) {
	skip := NewSkip(FrameSamples)
	if got := skip.Apply(numbered(0, FrameSamples)); len(got) != 0 {
		t.Fatalf("the skipped frame kept %d samples", len(got))
	}
	if got := skip.Apply(numbered(FrameSamples, FrameSamples)); len(got) != FrameSamples || got[0] != FrameSamples {
		t.Fatalf("the next frame kept %d samples starting at %d", len(got), got[0])
	}
}
