// Package opus reads the audio Alexa uploads on Fire OS 6: fixed 80-byte Opus
// packets of 16 kHz mono speech. It splits a stream into packets, decodes them
// with the device's own libopus, and drops the samples before a turn's speech.
// docs/voice.md has the measurements.
package opus

const (
	Rate         = 16000
	PacketBytes  = 80
	FrameSamples = Rate / 50
)

func Split(buf []byte) (packets [][]byte, rest []byte) {
	for len(buf) >= PacketBytes {
		packets = append(packets, buf[:PacketBytes:PacketBytes])
		buf = buf[PacketBytes:]
	}
	return packets, buf
}

type Skip struct {
	left int
}

func NewSkip(samples int) *Skip {
	return &Skip{left: max(samples, 0)}
}

func (s *Skip) Apply(pcm []int16) []int16 {
	if s.left >= len(pcm) {
		s.left -= len(pcm)
		return pcm[:0]
	}
	pcm = pcm[s.left:]
	s.left = 0
	return pcm
}
