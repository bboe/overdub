package esphome

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDecodeNoisePSK(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	key := base64.StdEncoding.EncodeToString(raw)

	got, err := DecodeNoisePSK(key)
	if err != nil {
		t.Fatalf("DecodeNoisePSK: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("decoded % x, want % x", got, raw)
	}

	// A space rather than a newline, because encoding/base64 skips \r and \n by
	// contract: a newline decodes with or without the TrimSpace, so asserting on
	// one asserts nothing. A stray space is what TrimSpace actually buys, and a
	// hand-edited key file is where one comes from.
	if _, err := DecodeNoisePSK(key + " \n"); err != nil {
		t.Errorf("trailing whitespace was rejected: %v", err)
	}
}

func TestDecodeNoisePSKRejects(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"not base64", "not base64 at all!"},
		{"empty", ""},
		{"too short", base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"too long", base64.StdEncoding.EncodeToString(make([]byte, 33))},
	}
	for _, tt := range tests {
		if _, err := DecodeNoisePSK(tt.key); err == nil {
			t.Errorf("%s: DecodeNoisePSK(%q) returned no error", tt.name, tt.key)
		}
	}
}

func TestNoiseFrameRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{{}, []byte("hello"), bytes.Repeat([]byte{0xab}, 4096)} {
		var buf bytes.Buffer
		if err := writeNoiseFrame(&buf, payload); err != nil {
			t.Fatalf("writeNoiseFrame: %v", err)
		}
		got, err := readNoiseFrame(bufio.NewReader(&buf), maxDataFrame)
		if err != nil {
			t.Fatalf("readNoiseFrame: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("round trip of %d bytes gave %d", len(payload), len(got))
		}
	}
}

func TestWriteNoiseFrameHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := writeNoiseFrame(&buf, make([]byte, 258)); err != nil {
		t.Fatalf("writeNoiseFrame: %v", err)
	}
	want := []byte{0x01, 0x01, 0x02}
	if got := buf.Bytes()[:3]; !bytes.Equal(got, want) {
		t.Errorf("header = % x, want % x", got, want)
	}
}

func TestReadNoiseFrameRejectsPlaintextLeadByte(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{0x00, 0x00, 0x01, 0x42}))
	if _, err := readNoiseFrame(r, maxDataFrame); err == nil {
		t.Fatal("readNoiseFrame accepted a plaintext lead byte")
	}
}

func TestReadNoiseFrameRejectsTruncated(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{0x01, 0x00, 0x04, 0xaa, 0xbb}))
	if _, err := readNoiseFrame(r, maxDataFrame); err == nil {
		t.Fatal("readNoiseFrame accepted a truncated payload")
	}
}

// Whether the stream is still at a frame boundary is what decides if a read can
// be taken over rather than dropped, so every return that has already taken the
// header off the socket has to say so. A return added later that does not is a
// resume three bytes into a frame.
func TestEveryErrorPastTheHeaderSaysTheStreamIsMidFrame(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes []byte
		limit int
		mid   bool
	}{
		{"nothing at all", nil, maxDataFrame, false},
		{"part of the header", []byte{0x01}, maxDataFrame, true},
		{"a plaintext lead byte", []byte{0x00, 0x00, 0x01, 0x42}, maxDataFrame, true},
		{"a length past the limit", []byte{0x01, 0xff, 0xff}, maxHandshakeFrame, true},
		{"a payload that stops short", []byte{0x01, 0x00, 0x04, 0xaa, 0xbb}, maxDataFrame, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(bytes.NewReader(tc.bytes))
			_, err := readNoiseFrame(r, tc.limit)
			if err == nil {
				t.Fatal("readNoiseFrame accepted it")
			}
			if got := errors.Is(err, errMidFrame); got != tc.mid {
				t.Errorf("errors.Is(err, errMidFrame) = %v, want %v for %v: %v", got, tc.mid, tc.bytes, err)
			}
		})
	}
}

func TestNoiseMACFailureStringIsExact(t *testing.T) {
	if noiseMACFailure != "Handshake MAC failure" {
		t.Errorf("noiseMACFailure = %q; Home Assistant string-matches this", noiseMACFailure)
	}
	if strings.TrimSpace(noiseMACFailure) != noiseMACFailure {
		t.Error("noiseMACFailure has surrounding whitespace")
	}
}

// endless supplies bytes forever, so a rejected length fails for the length
// rather than for running out of payload. The base commit's frame test passed
// with its cap deleted for exactly that reason.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0xaa
	}
	return len(p), nil
}

func TestReadNoiseFrameRejectsALengthOverTheLimit(t *testing.T) {
	for _, limit := range []int{maxHandshakeFrame, maxDataFrame} {
		head := []byte{leadEncrypted, byte((limit + 1) >> 8), byte(limit + 1)}
		r := bufio.NewReader(io.MultiReader(bytes.NewReader(head), endless{}))
		_, err := readNoiseFrame(r, limit)
		if err == nil {
			t.Fatalf("a %d byte frame was accepted against a limit of %d", limit+1, limit)
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("limit %d was refused for the wrong reason: %v", limit, err)
		}
	}
}

func TestReadNoiseFrameAcceptsTheLimitItself(t *testing.T) {
	var buf bytes.Buffer
	if err := writeNoiseFrame(&buf, make([]byte, maxDataFrame)); err != nil {
		t.Fatal(err)
	}
	got, err := readNoiseFrame(bufio.NewReader(&buf), maxDataFrame)
	if err != nil {
		t.Fatalf("a frame of exactly the limit was refused: %v", err)
	}
	if len(got) != maxDataFrame {
		t.Errorf("read %d bytes, want %d", len(got), maxDataFrame)
	}
}

// The handshake runs before a peer has proved anything, so what it can make the
// daemon reserve is the number that matters. ESPHome allows 128 bytes there and
// 32768 after; the product of the data bound and the connection cap has to fit
// a 512 MiB device besides.
func TestTheFrameBoundsAreESPHomesAndFitTheDevice(t *testing.T) {
	if maxHandshakeFrame != 128 || maxDataFrame != 32768 {
		t.Errorf("bounds are %d and %d, want ESPHome's 128 and 32768", maxHandshakeFrame, maxDataFrame)
	}
	if got := maxConns * maxDataFrame; got > 1<<20 {
		t.Errorf("%d connections of %d bytes is %d, more than a megabyte", maxConns, maxDataFrame, got)
	}
	if maxNoiseMessage+20 != maxDataFrame {
		t.Errorf("a largest message of %d does not fill a %d byte frame", maxNoiseMessage, maxDataFrame)
	}
}

// Deliberate, not an oversight: a peer that lies in the inner length field is
// read by the outer length, which is bounded, so both a larger and a smaller
// claim are harmless. Home Assistant's own client ignores the field for the same
// reason, and ESPHome's firmware only refuses a claim larger than the frame, so
// insisting the two agree would be stricter than either end of the real protocol.
func TestALyingInnerLengthIsIgnoredTheWayTheRealClientIgnoresIt(t *testing.T) {
	for _, said := range []uint16{0, 1, 0xffff} {
		psk := testPSK(t)
		c, err := dial(t, testServer(t, psk), psk)
		if err != nil {
			t.Fatal(err)
		}

		var p pb
		p.str(1, "probe")
		inner := make([]byte, 4, 4+len(p.b))
		binary.BigEndian.PutUint16(inner[0:2], uint16(msgHelloRequest))
		binary.BigEndian.PutUint16(inner[2:4], said)
		inner = append(inner, p.b...)

		sealed, err := c.out.Encrypt(nil, nil, inner)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeNoiseFrame(c.w, sealed); err != nil {
			t.Fatal(err)
		}
		if err := c.w.Flush(); err != nil {
			t.Fatal(err)
		}

		msgType, _, err := c.recv()
		if err != nil {
			t.Fatalf("a declared length of %d was refused: %v", said, err)
		}
		if msgType != msgHelloResponse {
			t.Errorf("declared %d: answered with type %d, want %d", said, msgType, msgHelloResponse)
		}
	}
}
