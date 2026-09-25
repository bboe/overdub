package sendspin

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"

	flacframe "github.com/mewkiz/flac/frame"
)

const (
	flacStreamInfoBytes = 34
	flacStreamInfoType  = 0x0
	flacLastBlock       = 0x80
	flacBlockHead       = 4
	flacHeadBytes       = 4
	flacSync            = 0xfff8
	flacRateFromHeader  = 0x0
	flacRate44kHz       = 0x9
	flacRate48kHz       = 0xa
	flacDepth16Bit      = 0x4
	flacTwoChannels     = 0x1
	flacLeftSide        = 0x8
	flacMidSide         = 0xa
	flacChunkFrames     = 4608 // the streamable subset's largest block at 48 kHz
)

var flacRates = map[int]byte{StreamRate: flacRate48kHz, BluetoothRate: flacRate44kHz}

var (
	flacMagic = []byte("fLaC")

	errFLACFormat = fmt.Errorf("%w: a FLAC frame in another format than its stream/start named",
		errTransport)
	errFLACFrame = fmt.Errorf("%w: a FLAC frame that does not decode", errTransport)
	errFLACLong  = fmt.Errorf("%w: a FLAC chunk past the %d frames of the subset's largest block",
		errTransport, flacChunkFrames)
)

func flacHeaderPlayable(encoded string, rate int) bool {
	header, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	if block, ok := bytes.CutPrefix(header, flacMagic); ok {
		if len(block) < flacBlockHead+flacStreamInfoBytes ||
			block[0]&^flacLastBlock != flacStreamInfoType ||
			int(block[1])<<16|int(block[2])<<8|int(block[3]) != flacStreamInfoBytes {
			return false
		}
		header = block[flacBlockHead : flacBlockHead+flacStreamInfoBytes]
	}
	if len(header) != flacStreamInfoBytes {
		return false
	}
	packed := binary.BigEndian.Uint64(header[10:]) // sample rate, channels, depth
	return int(packed>>44) == rate &&
		int(packed>>41&0x7)+1 == StreamChannels &&
		int(packed>>36&0x1f)+1 == StreamBitDepth
}

func flacFrameInStreamFormat(head []byte, rate int) bool {
	code := head[2] & 0x0f       // sample rate
	channels := head[3] >> 4     // channel assignment
	depth := head[3] >> 1 & 0x07 // sample size
	return (code == flacRateFromHeader || code == flacRates[rate]) &&
		(channels == flacTwoChannels || channels >= flacLeftSide && channels <= flacMidSide) &&
		depth == flacDepth16Bit
}

func decodeFLAC(audio []byte, rate int) ([]byte, error) {
	var pcm []byte
	budget := flacChunkFrames
	r := bytes.NewReader(audio)
	for r.Len() > 0 {
		head := audio[len(audio)-r.Len():]
		if len(head) < flacHeadBytes || binary.BigEndian.Uint16(head)&^1 != flacSync {
			return nil, errFLACFrame
		}
		if !flacFrameInStreamFormat(head, rate) {
			return nil, errFLACFormat
		}
		f, err := flacframe.New(r)
		if err != nil {
			return nil, errFLACFrame
		}
		n := int(f.BlockSize)
		if budget -= n; budget < 0 {
			return nil, errFLACLong
		}
		if err := f.Parse(); err != nil {
			return nil, errFLACFrame
		}
		pcm = slices.Grow(pcm, n*frameBytes)
		for i := range n {
			for _, sub := range f.Subframes {
				pcm = binary.LittleEndian.AppendUint16(pcm, uint16(sub.Samples[i]))
			}
		}
	}
	return pcm, nil
}
