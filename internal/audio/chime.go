// Package audio renders the chime, sounds it through OpenSL ES, and places a
// server's stream on the frame the player says it belongs on.
package audio

import "math"

const (
	ChimeRate     = 48000
	ChimeChannels = 2

	chimeSeconds = 0.40
	chimeGain    = 0.22

	chimeRamp = 0.005

	chimeFade = 0.10
)

func Rates() []int { return []int{ChimeRate, BluetoothRate} }

type note struct {
	freq  float64
	start float64
	dur   float64
}

var chimeNotes = []note{
	{freq: 880.0, start: 0.00, dur: 0.18},  // A5
	{freq: 1320.0, start: 0.18, dur: 0.22}, // E6
}

func chimePCM(rate int) []byte {
	frames := int(chimeSeconds * float64(rate))
	buf := make([]byte, frames*ChimeChannels*2)
	for i := range frames {
		t := float64(i) / float64(rate)
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
		for ch := range ChimeChannels {
			at := (i*ChimeChannels + ch) * 2
			buf[at] = byte(s)
			buf[at+1] = byte(s >> 8)
		}
	}
	return buf
}
