package sendspin

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/bboe/overdub/internal/flacread"
)

const flacChunkFrames = 4608 // the streamable subset's largest block at 48 kHz

var (
	errFLACFormat = fmt.Errorf("%w: a FLAC frame in another format than its stream/start named",
		errTransport)
	errFLACFrame = fmt.Errorf("%w: a FLAC frame that does not decode", errTransport)
	errFLACLong  = fmt.Errorf("%w: a FLAC chunk past the %d frames of the subset's largest block",
		errTransport, flacChunkFrames)
)

func flacHeaderPlayable(encoded string, rate int) bool {
	header, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && flacread.HeaderPlayable(header, rate)
}

type chunk struct {
	*bytes.Reader
	audio []byte
}

func (c chunk) Peek(n int) ([]byte, error) {
	rest := c.audio[len(c.audio)-c.Len():]
	if len(rest) < n {
		return rest, io.EOF
	}
	return rest[:n], nil
}

func decodeFLAC(audio []byte, rate int) ([]byte, error) {
	var pcm []byte
	var samples []int16
	budget := flacChunkFrames
	r := chunk{bytes.NewReader(audio), audio}
	for {
		var err error
		samples, err = flacread.Next(r, rate, budget, samples[:0])
		switch {
		case errors.Is(err, io.EOF):
			return pcm, nil
		case errors.Is(err, flacread.ErrFormat):
			return nil, errFLACFormat
		case errors.Is(err, flacread.ErrLong):
			return nil, errFLACLong
		case err != nil:
			return nil, errFLACFrame
		}
		budget -= len(samples) / StreamChannels
		pcm = slices.Grow(pcm, len(samples)*2)
		for _, v := range samples {
			pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
		}
	}
}
