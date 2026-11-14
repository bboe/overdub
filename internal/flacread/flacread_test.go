package flacread

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

func streamInfo(rate, channels, depth int) []byte {
	info := make([]byte, StreamInfoBytes)
	packed := uint64(rate)<<44 | uint64(channels-1)<<41 | uint64(depth-1)<<36
	binary.BigEndian.PutUint64(info[10:], packed)
	return info
}

func header(blocks ...[]byte) []byte {
	return slices.Concat(append([][]byte{Magic}, blocks...)...)
}

func TestAHeaderIsReadWithStreamInfoFirstAndTheRestSkipped(t *testing.T) {
	info := streamInfo(48000, Channels, Depth)
	comment := slices.Concat([]byte{LastBlock | 0x4, 0, 0, 5}, []byte("lavf6"))
	for _, tt := range []struct {
		name string
		in   []byte
		want error
	}{
		{"streaminfo then a comment", header([]byte{0, 0, 0, StreamInfoBytes}, info, comment), nil},
		{"streaminfo alone", header([]byte{LastBlock, 0, 0, StreamInfoBytes}, info), nil},
		{"a comment first", header(comment[:4], []byte("lavf6")), ErrHeader},
		{"another magic", slices.Concat([]byte("ID3\x04"), info), ErrHeader},
		{"44.1 kHz", header([]byte{LastBlock, 0, 0, StreamInfoBytes}, streamInfo(44100, 2, 16)), ErrFormat},
		{"mono", header([]byte{LastBlock, 0, 0, StreamInfoBytes}, streamInfo(48000, 1, 16)), ErrFormat},
		{"cut inside streaminfo", header([]byte{LastBlock, 0, 0, StreamInfoBytes}, info[:20]), ErrHeader},
		{"a block claiming 16 MB", header([]byte{0, 0, 0, StreamInfoBytes}, info,
			[]byte{LastBlock | 0x6, 0xff, 0xff, 0xff}, make([]byte, 64)), ErrHeader},
	} {
		if err := ReadHeader(bytes.NewReader(tt.in), 48000); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestAStreamCutInsideAFrameHeaderIsAFailure(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{0xff, 0xf8}))
	if _, err := Next(r, 48000, 4608, nil); !errors.Is(err, ErrFrame) {
		t.Errorf("2 bytes of a frame header then the end gave %v, want ErrFrame: a clean"+
			" end would hide a cut connection", err)
	}
	if _, err := Next(bufio.NewReader(bytes.NewReader(nil)), 48000, 4608, nil); err == nil ||
		err.Error() != "EOF" {
		t.Errorf("nothing at all gave %v, want io.EOF", err)
	}
}
