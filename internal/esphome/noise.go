package esphome

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/flynn/noise"
)

// The lead byte that opens every frame; ESPHome uses 0x00 for plaintext.
const leadEncrypted = 0x01

var noisePrologue = []byte("NoiseAPIInit\x00\x00")

// Hardcoded in Home Assistant's client, and mixed into the handshake hash by
// both ends: agreement rather than negotiation.
const noiseCipherName = "Noise_NNpsk0_25519_ChaChaPoly_SHA256"

// A package-level var so a test can build the name above from it.
var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

const noiseMACFailure = "Handshake MAC failure"

const noisePSKLen = 32

func DecodeNoisePSK(key string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil {
		return nil, fmt.Errorf("key is not valid base64: %w", err)
	}
	if len(raw) != noisePSKLen {
		return nil, fmt.Errorf("key decodes to %d bytes, want %d", len(raw), noisePSKLen)
	}
	return raw, nil
}

func writeNoiseFrame(w io.Writer, payload []byte) error {
	if len(payload) > 0xffff {
		return fmt.Errorf("noise frame of %d bytes exceeds the 16-bit length", len(payload))
	}
	head := []byte{leadEncrypted, byte(len(payload) >> 8), byte(len(payload))}
	if _, err := w.Write(append(head, payload...)); err != nil {
		return err
	}
	return nil
}

// What ESPHome's own firmware bounds a frame to before allocating for it, in
// api_frame_helper_noise.cpp. docs/architecture.md says why the 16-bit length
// field is not the bound.
const (
	maxHandshakeFrame = 128
	maxDataFrame      = 32768
)

// errMidFrame marks an error left with the stream no longer at a frame
// boundary, because part of one is already off the socket. Every return below
// that follows the header read carries it. docs/architecture.md says why such a
// read cannot be retried.
var errMidFrame = errors.New("stream left mid-frame")

func readNoiseFrame(r *bufio.Reader, limit int) ([]byte, error) {
	var head [3]byte
	if n, err := io.ReadFull(r, head[:]); err != nil {
		if n > 0 {
			return nil, fmt.Errorf("%w: %w", errMidFrame, err)
		}
		return nil, err
	}
	if head[0] != leadEncrypted {
		return nil, fmt.Errorf("%w: expected an encrypted frame, got lead byte 0x%02x", errMidFrame, head[0])
	}
	size := int(binary.BigEndian.Uint16(head[1:3]))
	if size > limit {
		return nil, fmt.Errorf("%w: frame of %d bytes exceeds the %d byte limit", errMidFrame, size, limit)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("%w: %w", errMidFrame, err)
	}
	return payload, nil
}

type noiseRW struct {
	sock   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	in     *noise.CipherState // decrypts what Home Assistant sends
	out    *noise.CipherState // encrypts what we send
}

func noiseAccept(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer, name string, psk []byte, deadline time.Time) (*noiseRW, error) {
	if len(psk) != noisePSKLen {
		return nil, fmt.Errorf("psk is %d bytes, want %d", len(psk), noisePSKLen)
	}

	handshake, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeNN,
		Initiator:             false,
		Prologue:              noisePrologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0, // psk0: mixed in before the first message
		Random:                rand.Reader,
	})
	if err != nil {
		return nil, fmt.Errorf("noise setup: %w", err)
	}

	// The caller's deadline, not a wait of our own: SECURITY.md gives one number
	// for the pre-authentication phase.
	_ = conn.SetWriteDeadline(deadline)
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()

	if hello, err := readNoiseFrame(reader, maxHandshakeFrame); err != nil {
		return nil, fmt.Errorf("client hello: %w", err)
	} else if len(hello) != 0 {
		return nil, fmt.Errorf("client hello carried %d unexpected bytes", len(hello))
	}

	serverHello := append([]byte{leadEncrypted}, name...)
	serverHello = append(serverHello, 0x00)
	if err := writeNoiseFrame(writer, serverHello); err != nil {
		return nil, err
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}

	msg, err := readNoiseFrame(reader, maxHandshakeFrame)
	if err != nil {
		return nil, fmt.Errorf("client handshake: %w", err)
	}
	if len(msg) == 0 {
		return nil, fmt.Errorf("client handshake was empty")
	}
	if msg[0] != 0x00 {
		return nil, fmt.Errorf("client handshake had a non-zero preamble")
	}
	if _, _, _, err := handshake.ReadMessage(nil, msg[1:]); err != nil {
		_ = writeNoiseFrame(writer, append([]byte{leadEncrypted}, noiseMACFailure...))
		_ = writer.Flush()
		return nil, fmt.Errorf("handshake rejected (wrong key?): %w", err)
	}

	serverHandshake, fromClient, toClient, err := handshake.WriteMessage([]byte{0x00}, nil)
	if err != nil {
		return nil, fmt.Errorf("noise write: %w", err)
	}
	if err := writeNoiseFrame(writer, serverHandshake); err != nil {
		return nil, err
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	if fromClient == nil || toClient == nil {
		return nil, fmt.Errorf("handshake finished without cipher states")
	}

	return &noiseRW{sock: conn, reader: reader, writer: writer, in: fromClient, out: toClient}, nil
}

func (n *noiseRW) read() (int, []byte, error) {
	frame, err := readNoiseFrame(n.reader, maxDataFrame)
	if err != nil {
		return 0, nil, err
	}
	plain, err := n.in.Decrypt(nil, nil, frame)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt: %w", err)
	}
	if len(plain) < 4 {
		return 0, nil, fmt.Errorf("decrypted message of %d bytes is too short", len(plain))
	}
	// plain[2:4] is read past rather than trusted, which is what aioesphomeapi
	// does with it. docs/architecture.md says why insisting it agree would be
	// stricter than either end of the real protocol.
	msgType := int(binary.BigEndian.Uint16(plain[0:2]))
	return msgType, plain[4:], nil
}

// What is left of a data frame once the inner header and Poly1305 tag are out.
const maxNoiseMessage = maxDataFrame - 20

func (n *noiseRW) write(msgType int, payload []byte) error {
	if len(payload) > maxNoiseMessage {
		return fmt.Errorf("message of %d bytes exceeds the %d byte frame", len(payload), maxNoiseMessage)
	}
	inner := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(inner[0:2], uint16(msgType))
	binary.BigEndian.PutUint16(inner[2:4], uint16(len(payload)))
	inner = append(inner, payload...)

	sealed, err := n.out.Encrypt(nil, nil, inner)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	n.sock.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := writeNoiseFrame(n.writer, sealed); err != nil {
		return err
	}
	return n.writer.Flush()
}
