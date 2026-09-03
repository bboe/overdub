package audio

import (
	"encoding/binary"
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
	want := int(chimeSeconds*chimeRate) * 2
	if got := len(chimePCM()); got != want {
		t.Errorf("got %d bytes, want %d", got, want)
	}
}

// A waveform that starts or ends away from zero is a click on the speaker,
// which is what the attack ramp and the decay envelope are for.
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

// Loud enough to hear over a room, and short of the ceiling the clamp imposes:
// a chime that rails is one the gain is wrong for rather than one that is loud.
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
