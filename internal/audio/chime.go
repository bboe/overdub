package audio

import "math"

const (
	chimeRate     = 48000
	chimeChannels = 1

	chimeSeconds = 0.40
	chimeGain    = 0.22

	chimeRamp = 0.005

	chimeFade = 0.10
)

type note struct {
	freq  float64
	start float64
	dur   float64
}

var chimeNotes = []note{
	{freq: 880.0, start: 0.00, dur: 0.18},  // A5
	{freq: 1320.0, start: 0.18, dur: 0.22}, // E6
}

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
