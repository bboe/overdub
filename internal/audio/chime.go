package audio

import "math"

// Generated rather than stored: the tree carries no audio asset, and nothing
// needs ffmpeg to regenerate one. 48kHz is the output's own rate, so nothing
// resamples this on the way to the codec.
const (
	chimeRate     = 48000
	chimeChannels = 1

	chimeSeconds = 0.40
	chimeGain    = 0.22

	// Each note is ramped at both ends, because a sine cut mid-cycle is a click
	// on the speaker. Five milliseconds is inaudible as a fade and enough to
	// remove the step, at the note boundary as much as at the edges.
	chimeRamp = 0.005

	// The recording holds its level for three quarters of its length and then
	// fades, rather than decaying like a bell from the first sample.
	chimeFade = 0.10
)

type note struct {
	freq  float64
	start float64
	dur   float64
}

// The clip this replaces was itself synthesised, and README.md recorded the
// ffmpeg line that made it: two sines, 880 Hz for 0.18s then 1320 Hz for 0.22s,
// concatenated and faded out over the last 0.10s. A DFT over each half of that
// clip reads 880.0 Hz and 1320.0 Hz, within two cents of A5 and E6 and with no
// partial above the fundamental carrying enough energy to matter, so these are
// the recording's own numbers rather than an approximation of them.
var chimeNotes = []note{
	{freq: 880.0, start: 0.00, dur: 0.18},  // A5
	{freq: 1320.0, start: 0.18, dur: 0.22}, // E6
}

// chimePCM renders signed 16-bit little-endian mono at chimeRate.
func chimePCM() []byte {
	frames := int(chimeSeconds * chimeRate)
	buf := make([]byte, frames*2)
	for i := range frames {
		t := float64(i) / chimeRate
		var v float64
		for _, n := range chimeNotes {
			if t < n.start || t >= n.start+n.dur {
				continue
			}
			age := t - n.start
			env := 1.0
			if age < chimeRamp {
				env = age / chimeRamp
			}
			if left := n.dur - age; left < chimeRamp {
				env = min(env, left/chimeRamp)
			}
			v += env * math.Sin(2*math.Pi*n.freq*age)
		}
		if left := chimeSeconds - t; left < chimeFade {
			v *= left / chimeFade
		}
		v = min(max(v*chimeGain, -1), 1)
		s := int16(v * math.MaxInt16)
		buf[i*2] = byte(s)
		buf[i*2+1] = byte(s >> 8)
	}
	return buf
}
