package sendspin

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	flacframe "github.com/mewkiz/flac/frame"
)

const flacVectorBlock = 4608

func flacPattern(n int) []int16 {
	out := make([]int16, n)
	x := uint32(1)
	for i := range out {
		x = x*1103515245 + 12345
		tri := i % 200
		if tri >= 100 {
			tri = 200 - tri
		}
		out[i] = int16(tri*160 - 8000 + int(x>>24) - 128)
	}
	return out
}

func flacVectorPCM() []byte {
	samples := append(flacPattern(2*flacVectorBlock), make([]int16, flacVectorBlock)...)
	pcm := make([]byte, 0, 2*len(samples))
	for _, v := range samples {
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
	}
	return pcm
}

func flacVector(t testing.TB) (header []byte, frames [][]byte) {
	t.Helper()
	b, err := os.ReadFile("testdata/aiosendspin-mono.flac")
	if err != nil {
		t.Fatal(err)
	}
	n := len(flacMagic)
	for {
		last := b[n]&flacLastBlock != 0
		n += 4 + int(b[n+1])<<16 | int(b[n+2])<<8 | int(b[n+3])
		if last {
			break
		}
	}
	header = b[:n]
	r := bytes.NewReader(b[n:])
	for r.Len() > 0 {
		start := len(b) - r.Len()
		if _, err := flacframe.Parse(r); err != nil {
			t.Fatalf("the vector's frame at byte %d: %v", start, err)
		}
		frames = append(frames, b[start:len(b)-r.Len()])
	}
	return header, frames
}

func flacHeaderB64(t testing.TB) string {
	t.Helper()
	header, _ := flacVector(t)
	return base64.StdEncoding.EncodeToString(header)
}

func flacStarted(t *testing.T, header string) *Session {
	t.Helper()
	s := held()
	p := ours()
	p.Codec, p.CodecHeader = codecFLAC, header
	startWith(t, s, &p)
	return s
}

func streamInfo(rate, channels, bits int) []byte {
	b := make([]byte, flacStreamInfoBytes)
	binary.BigEndian.PutUint16(b[0:], flacVectorBlock)
	binary.BigEndian.PutUint16(b[2:], flacVectorBlock)
	binary.BigEndian.PutUint64(b[10:],
		uint64(rate)<<44|uint64(channels-1)<<41|uint64(bits-1)<<36)
	return b
}

func wrapped(info []byte) []byte {
	return slices.Concat(flacMagic, []byte{flacLastBlock, 0, 0, byte(len(info))}, info)
}

func flacCRC8(b []byte) byte {
	var c byte
	for _, x := range b {
		c ^= x
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func flacCRC16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c ^= uint16(x) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func constantFrame(rate byte, block int, value int16) []byte {
	f := []byte{0xff, 0xf8,
		0x70 | rate, // block size in 16 bits after the header
		0x08,        // mono, 16-bit
		0x00}        // frame number 0
	f = binary.BigEndian.AppendUint16(f, uint16(block-1))
	f = append(f, flacCRC8(f))
	f = append(f, 0x00) // a constant subframe
	f = binary.BigEndian.AppendUint16(f, uint16(value))
	return binary.BigEndian.AppendUint16(f, flacCRC16(f))
}

func TestAFLACStreamDecodesToTheSamplesThatWereEncoded(t *testing.T) {
	_, frames := flacVector(t)
	s := flacStarted(t, flacHeaderB64(t))
	if !s.Streaming() {
		t.Fatal("a flac stream/start carrying the reference encoder's own header was refused")
	}
	var pcm []byte
	for i, f := range frames {
		c, err := s.AudioChunk(chunkBody(int64(1000+i), f))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if c.ServerTime != int64(1000+i) {
			t.Errorf("frame %d came back stamped %d", i, c.ServerTime)
		}
		if c.Frames() != flacVectorBlock {
			t.Errorf("frame %d decoded to %d frames, want %d", i, c.Frames(), flacVectorBlock)
		}
		pcm = append(pcm, c.PCM...)
	}
	if !bytes.Equal(pcm, flacVectorPCM()) {
		t.Error("the decoded audio differs from what was encoded; FLAC is lossless, so" +
			" any difference is a decoding error the ear hears as noise")
	}
}

func TestSeveralFLACFramesInOneChunkAllDecode(t *testing.T) {
	var chunk, want []byte
	for v := range int16(4) {
		chunk = append(chunk, constantFrame(flacRate48kHz, flacChunkFrames/4, 100*v)...)
		for range flacChunkFrames / 4 {
			want = binary.LittleEndian.AppendUint16(want, uint16(100*v))
		}
	}
	s := flacStarted(t, flacHeaderB64(t))
	c, err := s.AudioChunk(chunkBody(1, chunk))
	if err != nil {
		t.Fatalf("AudioChunk: %v", err)
	}
	if !bytes.Equal(c.PCM, want) {
		t.Errorf("a chunk of 4 frames decoded to %d bytes, want %d; a server may pack"+
			" more than one frame into a chunk", len(c.PCM), len(want))
	}
}

func TestAFLACFrameInAnotherFormatIsRefusedBeforeTheDecoderSeesIt(t *testing.T) {
	_, frames := flacVector(t)
	for _, tc := range []struct {
		what string
		edit func(b []byte)
	}{
		{"24 kHz, a rate the decoder logs", func(b []byte) { b[2] = b[2]&0xf0 | 0x7 }},
		{"176.4 kHz, a rate the decoder logs", func(b []byte) { b[2] = b[2]&0xf0 | 0x2 }},
		{"44.1 kHz", func(b []byte) { b[2] = b[2]&0xf0 | 0x9 }},
		{"a bit depth left to STREAMINFO, which the decoder reads as 0 bits",
			func(b []byte) { b[3] &^= 0x0e }},
		{"24-bit", func(b []byte) { b[3] = b[3]&^0x0e | 0x6<<1 }},
		{"stereo", func(b []byte) { b[3] = b[3]&0x0f | 0x1<<4 }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			var out lockedLog
			was := log.Writer()
			log.SetOutput(&out)
			defer log.SetOutput(was)

			s := flacStarted(t, flacHeaderB64(t))
			f := slices.Clone(frames[0])
			tc.edit(f)
			if _, err := s.AudioChunk(chunkBody(1, f)); !errors.Is(err, errFLACFormat) {
				t.Errorf("a frame at %s came back as %v, want the format refusal", tc.what, err)
			}
			if out.String() != "" {
				t.Errorf("a frame from the server wrote to the daemon's log:\n%s", out.String())
			}
		})
	}
}

func TestABrokenFLACFrameIsRefusedWithOneMessage(t *testing.T) {
	_, frames := flacVector(t)
	f := frames[0]
	flip := func(i int) []byte {
		b := slices.Clone(f)
		b[i] ^= 0x10
		return b
	}
	said := map[string]bool{}
	for _, tc := range []struct {
		what  string
		audio []byte
	}{
		{"a flipped bit early in the audio", flip(len(f) / 4)},
		{"a flipped bit late in the audio", flip(3 * len(f) / 4)},
		{"a flipped bit in the header's frame number", flip(4)},
		{"the first half of a frame", f[:len(f)/2]},
		{"a frame cut before its CRC", f[:len(f)-1]},
		{"a frame followed by 3 stray bytes", slices.Concat(f, []byte{0xff, 0xf8, 0xc9})},
		{"no sync code", slices.Concat([]byte{0, 0}, f[2:])},
		{"zeros where a frame should start", make([]byte, 16)},
	} {
		s := flacStarted(t, flacHeaderB64(t))
		_, err := s.AudioChunk(chunkBody(1, tc.audio))
		if !errors.Is(err, errFLACFrame) {
			t.Errorf("%s came back as %v, want the decode refusal", tc.what, err)
			continue
		}
		said[err.Error()] = true
	}
	if len(said) > 1 {
		t.Errorf("broken frames were refused with %d different messages; the run loop logs"+
			" each distinct one, so a number from the frame in the text fills the list"+
			" and silences the connection's log", len(said))
	}
}

func TestAFLACStreamStartNeedsAHeaderForItsOwnFormat(t *testing.T) {
	good := streamInfo(StreamRate, StreamChannels, StreamBitDepth)
	for _, tc := range []struct {
		what   string
		header string
		plays  bool
	}{
		{"the reference encoder's header", flacHeaderB64(t), true},
		{"a STREAMINFO block with its fLaC wrapper", b64(wrapped(good)), true},
		{"a bare STREAMINFO block", b64(good), true},
		{"no header", "", false},
		{"a header that is not base64", "fLaC!!", false},
		{"a STREAMINFO at 44.1 kHz", b64(wrapped(streamInfo(44100, 1, 16))), false},
		{"a stereo STREAMINFO", b64(wrapped(streamInfo(StreamRate, 2, 16))), false},
		{"a 5-channel STREAMINFO", b64(wrapped(streamInfo(StreamRate, 5, 16))), false},
		{"a 24-bit STREAMINFO", b64(wrapped(streamInfo(StreamRate, 1, 24))), false},
		{"a 32-bit STREAMINFO", b64(wrapped(streamInfo(StreamRate, 1, 32))), false},
		{"a STREAMINFO cut short", b64(wrapped(good)[:20]), false},
		{"a bare block of the wrong length", b64(good[:33]), false},
		{"a bare block 1 byte long", b64(append(slices.Clone(good), 0)), false},
		{"another magic", b64(slices.Concat([]byte("fLaX"), wrapped(good)[4:])), false},
		{"STREAMINFO not last, then more blocks", b64(slices.Concat(flacMagic,
			[]byte{0, 0, 0, flacStreamInfoBytes}, good, []byte{0x81, 0, 0, 0})), true},
		{"a PICTURE block first", b64(slices.Concat(flacMagic,
			[]byte{0x86, 0, 0, flacStreamInfoBytes}, good)), false},
		{"a STREAMINFO block of the wrong length", b64(slices.Concat(flacMagic,
			[]byte{flacLastBlock, 0, 0, flacStreamInfoBytes + 1}, good, []byte{0})), false},
	} {
		s := flacStarted(t, tc.header)
		if s.Streaming() != tc.plays {
			t.Errorf("%s: streaming = %v, want %v", tc.what, s.Streaming(), tc.plays)
		}
		p := ours()
		p.Codec, p.CodecHeader = codecFLAC, tc.header
		if said := p.String(); strings.Contains(said, "cannot read") == tc.plays {
			t.Errorf("%s is described as %q", tc.what, said)
		}
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestAFLACHeaderIsReadWithoutTheSizesItClaims(t *testing.T) {
	picture := slices.Concat(flacMagic, []byte{0x86, 0, 0, 0x24}, // PICTURE, 36 bytes
		[]byte{0, 0, 0, 3},                     // picture type
		[]byte{0, 0, 0, 0}, []byte{0, 0, 0, 0}, // no MIME type, no description
		make([]byte, 16),      // size, depth, colours
		[]byte{0x08, 0, 0, 0}) // 128 MB of data to come
	seektable := slices.Concat(flacMagic, []byte{0x03, 0xff, 0xff, 0xff}, make([]byte, 32))
	for what, header := range map[string][]byte{
		"a PICTURE block claiming 128 MB":  picture,
		"a SEEKTABLE block claiming 16 MB": seektable,
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		playable := flacHeaderPlayable(b64(header))
		runtime.ReadMemStats(&after)
		if playable {
			t.Errorf("%s was taken for a STREAMINFO", what)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Errorf("%s: reading a %d-byte header allocated %d MB; the Dot has about"+
				" 100 MB free, and every stream/start is read", what, len(header), grew>>20)
		}
	}
}

func TestAFLACFrameLeavingItsRateToTheHeaderDecodes(t *testing.T) {
	s := flacStarted(t, flacHeaderB64(t))
	c, err := s.AudioChunk(chunkBody(1, constantFrame(flacRateFromHeader, 16, 1234)))
	if err != nil {
		t.Fatalf("a frame naming no rate of its own was refused: %v", err)
	}
	for i := range c.Frames() {
		if v := int16(binary.LittleEndian.Uint16(c.PCM[2*i:])); v != 1234 {
			t.Fatalf("frame %d decoded as %d, want 1234", i, v)
		}
	}
	if c.Frames() != 16 {
		t.Errorf("a 16-sample frame decoded to %d frames", c.Frames())
	}
}

func TestAFLACChunkDecodesToNoMoreThanTheSubsetsLargestBlock(t *testing.T) {
	biggest := constantFrame(flacRate48kHz, flacVectorBlock, 7)
	s := flacStarted(t, flacHeaderB64(t))
	c, err := s.AudioChunk(chunkBody(1, biggest))
	if err != nil {
		t.Fatalf("the largest block the streamable subset allows at 48 kHz, which"+
			" aiosendspin sends, was refused: %v", err)
	}
	if c.Frames() != flacVectorBlock {
		t.Errorf("it decoded to %d frames, want %d", c.Frames(), flacVectorBlock)
	}
	if _, err := s.AudioChunk(chunkBody(1, slices.Concat(biggest, biggest))); !errors.Is(err, errFLACLong) {
		t.Errorf("2 of them in one chunk came back as %v", err)
	}
	one := constantFrame(flacRate48kHz, 1, 7)
	if _, err := s.AudioChunk(chunkBody(1, slices.Concat(biggest, one))); !errors.Is(err, errFLACLong) {
		t.Errorf("a chunk of %d frames came back as %v", flacVectorBlock+1, err)
	}
	broken := slices.Clone(biggest)
	broken[len(broken)-1] ^= 0xff
	if _, err := s.AudioChunk(chunkBody(1, slices.Concat(biggest, broken))); !errors.Is(err, errFLACLong) {
		t.Errorf("a second frame past the budget came back as %v; it was decoded before"+
			" its size was weighed, which spends a frame's work the chunk had no room for", err)
	}
	tiny := constantFrame(flacRate48kHz, 1<<16-1, 0)
	if _, err := s.AudioChunk(chunkBody(1, tiny)); !errors.Is(err, errFLACLong) {
		t.Errorf("a %d-byte frame claiming 65535 samples came back as %v; on a Dot it"+
			" costs 4.0 ms to decode, 12 times a subset block", len(tiny), err)
	}
	bomb := slices.Repeat(tiny, (maxMessage-1-chunkStampBytes)/len(tiny))
	if _, err := s.AudioChunk(chunkBody(1, bomb)); !errors.Is(err, errFLACLong) {
		t.Errorf("a %d-byte chunk of %d-byte frames came back as %v; decoded, it is"+
			" %d MB of audio from one message", len(bomb), len(tiny), err,
			len(bomb)/len(tiny)*(1<<16-1)*frameBytes>>20)
	}
}

func TestAPCMStreamAfterAFLACOneTakesPCMAgain(t *testing.T) {
	s := flacStarted(t, flacHeaderB64(t))
	if _, err := s.AudioChunk(chunkBody(1, []byte{1, 2})); err == nil {
		t.Error("raw samples on a flac stream were accepted as a frame")
	}
	p := ours()
	startWith(t, s, &p)
	c, err := s.AudioChunk(chunkBody(1, []byte{1, 2}))
	if err != nil {
		t.Fatalf("a pcm stream after a flac one still decodes flac: %v", err)
	}
	if !bytes.Equal(c.PCM, []byte{1, 2}) {
		t.Errorf("the pcm came back as %v", c.PCM)
	}
}

func FuzzFLACChunk(f *testing.F) {
	_, frames := flacVector(f)
	for _, fr := range frames {
		f.Add(fr)
	}
	f.Add(slices.Concat(frames...))
	f.Add(constantFrame(flacRate48kHz, flacChunkFrames, 0))
	f.Fuzz(func(t *testing.T, audio []byte) {
		pcm, err := decodeFLAC(audio)
		switch {
		case err == nil && len(pcm)%frameBytes != 0:
			t.Fatalf("decoded %d bytes, which is not whole frames", len(pcm))
		case len(pcm) > flacChunkFrames*frameBytes:
			t.Fatalf("decoded %d bytes from %d", len(pcm), len(audio))
		case err != nil && !errors.Is(err, errFLACFormat) && !errors.Is(err, errFLACFrame) &&
			!errors.Is(err, errFLACLong):
			t.Fatalf("refused with %v, which is not one of the 3 fixed messages", err)
		}
	})
}
