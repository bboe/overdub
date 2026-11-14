// Package flacread reads stereo 16-bit FLAC for Sendspin and for clips. It
// reads the header and each frame's codes by hand before mewkiz/flac sees
// them, because the library allocates what a metadata block claims and logs
// for some rate codes. docs/sendspin.md has the traps.
package flacread

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"

	flacframe "github.com/mewkiz/flac/frame"
)

const (
	Channels = 2
	Depth    = 16

	StreamInfoBytes = 34
	streamInfoType  = 0x0
	LastBlock       = 0x80
	blockHead       = 4
	MaxMetadata     = 1 << 20

	headBytes      = 4
	frameSync      = 0xfff8
	RateFromHeader = 0x0
	Rate44kHz      = 0x9
	Rate48kHz      = 0xa
	depth16Bit     = 0x4
	TwoChannels    = 0x1
	LeftSide       = 0x8
	MidSide        = 0xa
)

var (
	Magic = []byte("fLaC")

	rateCodes = map[int]byte{44100: Rate44kHz, 48000: Rate48kHz}

	ErrHeader = errors.New("flac: the header does not parse")
	ErrFormat = errors.New("flac: a frame in another format than the stream's")
	ErrFrame  = errors.New("flac: a frame that does not decode")
	ErrLong   = errors.New("flac: a frame of more samples than allowed")
)

func playable(info []byte, rate int) bool {
	packed := binary.BigEndian.Uint64(info[10:]) // sample rate, channels, depth
	return int(packed>>44) == rate && int(packed>>41&0x7)+1 == Channels &&
		int(packed>>36&0x1f)+1 == Depth
}

func streamInfoBlock(head []byte) bool {
	return head[0]&^LastBlock == streamInfoType &&
		int(head[1])<<16|int(head[2])<<8|int(head[3]) == StreamInfoBytes
}

func HeaderPlayable(header []byte, rate int) bool {
	if block, ok := bytes.CutPrefix(header, Magic); ok {
		if len(block) < blockHead+StreamInfoBytes || !streamInfoBlock(block) {
			return false
		}
		header = block[blockHead : blockHead+StreamInfoBytes]
	}
	return len(header) == StreamInfoBytes && playable(header, rate)
}

func ReadHeader(r io.Reader, rate int) error {
	head := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, head); err != nil || !bytes.Equal(head, Magic) {
		return ErrHeader
	}
	var block [blockHead]byte
	if _, err := io.ReadFull(r, block[:]); err != nil || !streamInfoBlock(block[:]) {
		return ErrHeader
	}
	info := make([]byte, StreamInfoBytes)
	if _, err := io.ReadFull(r, info); err != nil {
		return ErrHeader
	}
	if !playable(info, rate) {
		return ErrFormat
	}
	for seen := 0; block[0]&LastBlock == 0; {
		if _, err := io.ReadFull(r, block[:]); err != nil {
			return ErrHeader
		}
		size := int(block[1])<<16 | int(block[2])<<8 | int(block[3])
		if seen += blockHead + size; seen > MaxMetadata {
			return ErrHeader
		}
		if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
			return ErrHeader
		}
	}
	return nil
}

func inFormat(head []byte, rate int) bool {
	code := head[2] & 0x0f       // sample rate
	channels := head[3] >> 4     // channel assignment
	depth := head[3] >> 1 & 0x07 // sample size
	return (code == RateFromHeader || code == rateCodes[rate]) &&
		(channels == TwoChannels || channels >= LeftSide && channels <= MidSide) &&
		depth == depth16Bit
}

type Source interface {
	io.Reader
	Peek(n int) ([]byte, error)
}

func Next(r Source, rate, most int, pcm []int16) ([]int16, error) {
	head, err := r.Peek(headBytes)
	if len(head) < headBytes {
		if err == io.EOF && len(head) == 0 {
			return nil, io.EOF
		}
		if err == io.EOF {
			return nil, ErrFrame
		}
		return nil, err
	}
	if binary.BigEndian.Uint16(head)&^1 != frameSync {
		return nil, ErrFrame
	}
	if !inFormat(head, rate) {
		return nil, ErrFormat
	}
	f, err := flacframe.New(r)
	if err != nil {
		return nil, ErrFrame
	}
	n := int(f.BlockSize)
	if n > most {
		return nil, ErrLong
	}
	if err := f.Parse(); err != nil {
		return nil, ErrFrame
	}
	pcm = slices.Grow(pcm, n*Channels)
	for i := range n {
		for _, sub := range f.Subframes {
			pcm = append(pcm, int16(sub.Samples[i]))
		}
	}
	return pcm, nil
}
