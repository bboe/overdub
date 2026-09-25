package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func samples(t *testing.T, rate int) []int16 {
	t.Helper()
	raw := chimePCM(rate)
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
	for _, rate := range Rates() {
		want := int(chimeSeconds*float64(rate)) * ChimeChannels * 2
		if got := len(chimePCM(rate)); got != want {
			t.Errorf("at %d Hz got %d bytes, want %d", rate, got, want)
		}
	}
}

func TestChimeStartsAndEndsSilent(t *testing.T) {
	for _, rate := range Rates() {
		s := samples(t, rate)
		const quiet = 300 // of 32767, about -40dB
		if abs16(s[0]) > quiet {
			t.Errorf("at %d Hz the first sample %d is not silence: the onset would click",
				rate, s[0])
		}
		if abs16(s[len(s)-1]) > quiet {
			t.Errorf("at %d Hz the last sample %d is not silence: the tail would click",
				rate, s[len(s)-1])
		}
	}
}

func TestChimeIsAudibleWithoutRailing(t *testing.T) {
	for _, rate := range Rates() {
		s := samples(t, rate)
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
			t.Errorf("at %d Hz the peak %d is too quiet to hear", rate, peak)
		}
		if railed > 0 {
			t.Errorf("at %d Hz %d samples are at full scale, so the gain clips", rate, railed)
		}
	}
}

func TestTheChimeSoundsTheSameInEveryChannel(t *testing.T) {
	for _, rate := range Rates() {
		s := samples(t, rate)
		for i := 0; i < len(s); i += ChimeChannels {
			for ch := 1; ch < ChimeChannels; ch++ {
				if s[i+ch] != s[i] {
					t.Fatalf("at %d Hz frame %d is %v; the chime is one voice, and a speaker"+
						" that sums the channels would play it at the wrong level", rate,
						i/ChimeChannels, s[i:i+ChimeChannels])
				}
			}
		}
	}
}

func TestTheChimeIsTheSameToneAtEveryRate(t *testing.T) {
	for _, rate := range Rates() {
		s := samples(t, rate)
		n := int(chimeNotes[0].dur * float64(rate))
		crossings := 0
		for i := ChimeChannels; i < n*ChimeChannels; i += ChimeChannels {
			if (s[i-ChimeChannels] < 0) != (s[i] < 0) {
				crossings++
			}
		}
		if got := float64(crossings) / 2 / chimeNotes[0].dur; math.Abs(got-chimeNotes[0].freq) > 20 {
			t.Errorf("at %d Hz the first note sounds at about %.0f Hz, want %.0f: a clip"+
				" rendered for one rate and played at another is off pitch", rate, got,
				chimeNotes[0].freq)
		}
	}
}

func abs16(v int16) int {
	if v < 0 {
		return -int(v)
	}
	return int(v)
}
