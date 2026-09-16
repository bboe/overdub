package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func samples(t *testing.T) []int16 {
	t.Helper()
	raw := chimePCM()
	if len(raw)%2 != 0 {
		t.Fatalf("got %d bytes, which is not whole 16-bit samples", len(raw))
	}
	s := make([]int16, len(raw)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return s
}

func TestChimeIsTheLengthItClaims(t *testing.T) {
	want := int(chimeSeconds*ChimeRate) * 2
	if got := len(chimePCM()); got != want {
		t.Errorf("got %d bytes, want %d", got, want)
	}
}

func TestChimeStartsAndEndsSilent(t *testing.T) {
	s := samples(t)
	const quiet = 300 // of 32767, about -40dB
	if abs16(s[0]) > quiet {
		t.Errorf("first sample %d is not silence: the onset would click", s[0])
	}
	if abs16(s[len(s)-1]) > quiet {
		t.Errorf("last sample %d is not silence: the tail would click", s[len(s)-1])
	}
}

func TestChimeIsAudibleWithoutRailing(t *testing.T) {
	s := samples(t)
	var peak, railed int
	for _, v := range s {
		if a := abs16(v); a > peak {
			peak = a
		}
		if abs16(v) >= 32767 {
			railed++
		}
	}
	if peak < 3000 {
		t.Errorf("peak %d is too quiet to hear", peak)
	}
	if railed > 0 {
		t.Errorf("%d samples are at full scale, so the gain clips", railed)
	}
}

func abs16(v int16) int {
	if v < 0 {
		return -int(v)
	}
	return int(v)
}

func TestFeedWritesEveryByteOnceAndInOrder(t *testing.T) {
	clip := chimePCM()
	var got []byte
	err := feed(clip, func(pcm []byte) (int, error) {
		n := min(4096, len(pcm))
		got = append(got, pcm[:n]...)
		return n, nil
	})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if !bytes.Equal(got, clip) {
		t.Errorf("the player was handed %d bytes of a %d byte clip, or handed them"+
			" out of order", len(got), len(clip))
	}
}

func TestFeedStopsWhenThePlayerTakesNothing(t *testing.T) {
	calls := 0
	err := feed(chimePCM(), func([]byte) (int, error) {
		calls++
		return 0, nil
	})
	if err == nil {
		t.Fatal("a player taking nothing is a full queue the chime cannot wait out")
	}
	if calls != 1 {
		t.Errorf("feed called the player %d times against a queue that took nothing,"+
			" so it spins rather than reporting", calls)
	}
}

func TestFeedRefusesAPlayerClaimingMoreThanItWasOffered(t *testing.T) {
	err := feed(chimePCM(), func(pcm []byte) (int, error) {
		return len(pcm) + 1, nil
	})
	if err == nil {
		t.Error("a count past the end of the clip was believed, which slices out of range")
	}
}

func TestFeedReportsWhatThePlayerReported(t *testing.T) {
	want := errors.New("the player would not take the samples")
	if err := feed(chimePCM(), func([]byte) (int, error) { return 0, want }); !errors.Is(err, want) {
		t.Errorf("feed returned %v, want the player's own error", err)
	}
}
