package esphome

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
)

const testTimeout = 5 * time.Second

func testServer(t *testing.T, psk []byte) *Server {
	t.Helper()
	return NewServer("kitchen", "Echo Dot (2nd Generation)", "", "00:00:5E:00:53:2A", psk)
}

func testPSK(t *testing.T) []byte {
	t.Helper()
	psk := make([]byte, noisePSKLen)
	if _, err := rand.Read(psk); err != nil {
		t.Fatal(err)
	}
	return psk
}

type client struct {
	conn     net.Conn
	r        *bufio.Reader
	w        *bufio.Writer
	out, in  *noise.CipherState
	hostName string
}

func dial(t *testing.T, s *Server, psk []byte) (*client, error) {
	t.Helper()
	near, far := net.Pipe()
	done := make(chan struct{})
	go func() {
		s.serveConn(far)
		close(done)
	}()
	t.Cleanup(func() {
		near.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serveConn was still running after its client closed")
		}
	})

	return handshakeOver(t, near, psk)
}

func handshakeOver(t *testing.T, near net.Conn, psk []byte) (*client, error) {
	t.Helper()
	_ = near.SetDeadline(time.Now().Add(testTimeout))
	c := &client{conn: near, r: bufio.NewReader(near), w: bufio.NewWriter(near)}

	handshake, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeNN,
		Initiator:             true,
		Prologue:              noisePrologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
		Random:                rand.Reader,
	})
	if err != nil {
		return nil, err
	}
	if err := writeNoiseFrame(c.w, nil); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	hello, err := readNoiseFrame(c.r, maxDataFrame)
	if err != nil {
		return nil, err
	}
	if len(hello) > 1 {
		c.hostName = string(hello[1 : len(hello)-1])
	}

	msg, _, _, err := handshake.WriteMessage([]byte{0x00}, nil)
	if err != nil {
		return nil, err
	}
	if err := writeNoiseFrame(c.w, msg); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}

	reply, err := readNoiseFrame(c.r, maxDataFrame)
	if err != nil {
		return nil, err
	}
	if len(reply) == 0 || reply[0] != 0x00 {
		return c, errRefused{string(reply[1:])}
	}
	_, cs0, cs1, err := handshake.ReadMessage(nil, reply[1:])
	if err != nil {
		return nil, err
	}
	c.out, c.in = cs0, cs1
	return c, nil
}

type errRefused struct{ said string }

func (e errRefused) Error() string { return "refused: " + e.said }

func (c *client) send(msgType int, payload []byte) error {
	inner := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(inner[0:2], uint16(msgType))
	binary.BigEndian.PutUint16(inner[2:4], uint16(len(payload)))
	inner = append(inner, payload...)
	sealed, err := c.out.Encrypt(nil, nil, inner)
	if err != nil {
		return err
	}
	if err := writeNoiseFrame(c.w, sealed); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *client) recv() (int, []byte, error) {
	frame, err := readNoiseFrame(c.r, maxDataFrame)
	if err != nil {
		return 0, nil, err
	}
	plain, err := c.in.Decrypt(nil, nil, frame)
	if err != nil {
		return 0, nil, err
	}
	if len(plain) < 4 {
		return 0, nil, fmt.Errorf("reply of %d bytes has no inner header", len(plain))
	}
	if said := int(binary.BigEndian.Uint16(plain[2:4])); said != len(plain)-4 {
		return 0, nil, fmt.Errorf("inner header says %d bytes, payload is %d", said, len(plain)-4)
	}
	return int(binary.BigEndian.Uint16(plain[0:2])), plain[4:], nil
}

func TestAnEmptyHandshakeMessageIsRefusedRatherThanIndexed(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.handshakeWait = time.Second
	near := serveOne(t, s)
	_ = near.SetDeadline(time.Now().Add(testTimeout))

	w := bufio.NewWriter(near)
	r := bufio.NewReader(near)
	if err := writeNoiseFrame(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := readNoiseFrame(r, maxDataFrame); err != nil {
		t.Fatalf("server hello: %v", err)
	}
	if err := writeNoiseFrame(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := readNoiseFrame(r, maxDataFrame); err == nil {
		t.Fatal("an empty handshake message was answered rather than refused")
	}
}

func TestAShortDecryptedMessageIsRefusedRatherThanSliced(t *testing.T) {
	psk := testPSK(t)
	c, err := dial(t, testServer(t, psk), psk)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.out.Encrypt(nil, nil, []byte{0x00})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNoiseFrame(c.w, sealed); err != nil {
		t.Fatal(err)
	}
	if err := c.w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err == nil {
		t.Fatal("a one-byte message was answered rather than refused")
	}
}

func TestAPeerThatNeverReadsDoesNotHoldItsSlot(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.handshakeWait = 300 * time.Millisecond
	near, far := net.Pipe()
	served := make(chan struct{})
	go func() {
		s.serveConn(far)
		close(served)
	}()
	defer near.Close()

	go func() {
		w := bufio.NewWriter(near)
		_ = writeNoiseFrame(w, nil)
		_ = w.Flush()
	}()

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("a peer that never read held its slot past the handshake wait")
	}
}

func TestHandshakeWithTheRightKeyReachesTheAPI(t *testing.T) {
	psk := make([]byte, noisePSKLen)
	if _, err := rand.Read(psk); err != nil {
		t.Fatal(err)
	}
	c, err := dial(t, testServer(t, psk), psk)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if c.hostName != "kitchen" {
		t.Errorf("ServerHello named %q, want kitchen", c.hostName)
	}

	var hello pb
	hello.str(1, "test")
	if err := c.send(msgHelloRequest, hello.b); err != nil {
		t.Fatal(err)
	}
	msgType, payload, err := c.recv()
	if err != nil {
		t.Fatal(err)
	}
	if msgType != msgHelloResponse {
		t.Fatalf("got message type %d, want HelloResponse (%d)", msgType, msgHelloResponse)
	}
	var major, minor uint64
	pbWalk(payload, func(f pbField) {
		switch f.field {
		case 1:
			major = f.num
		case 2:
			minor = f.num
		}
	})
	if major != 1 || minor != 12 {
		t.Errorf("API version %d.%d, want 1.12", major, minor)
	}
}

func TestHandshakeWithTheWrongKeyIsRefusedByName(t *testing.T) {
	server, client := make([]byte, noisePSKLen), make([]byte, noisePSKLen)
	if _, err := rand.Read(server); err != nil {
		t.Fatal(err)
	}
	copy(client, server)
	client[0] ^= 0xff

	_, err := dial(t, testServer(t, server), client)
	refused, ok := err.(errRefused)
	if !ok {
		t.Fatalf("wrong key gave %v, want a refusal", err)
	}
	if refused.said != noiseMACFailure {
		t.Errorf("refused with %q, want %q", refused.said, noiseMACFailure)
	}
}

func TestAServerWithNoKeyRefusesEveryone(t *testing.T) {
	c, err := dial(t, testServer(t, nil), nil)
	if err == nil && c != nil && c.out != nil {
		t.Fatal("a keyless server gave a session to a peer with no key")
	}
}

func TestAServerWithAShortKeyRefusesEveryone(t *testing.T) {
	short := make([]byte, noisePSKLen-1)
	c, err := dial(t, testServer(t, short), testPSK(t))
	if err == nil && c != nil && c.out != nil {
		t.Fatal("a server holding a short key produced a session")
	}
}

func TestARequestSentInsteadOfAHandshakeIsRefused(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)

	near, far := net.Pipe()
	served := make(chan struct{})
	go func() {
		s.serveConn(far)
		close(served)
	}()
	defer func() {
		near.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("serveConn was still running after its client closed")
		}
	}()
	_ = near.SetDeadline(time.Now().Add(testTimeout))

	w := bufio.NewWriter(near)
	r := bufio.NewReader(near)

	_ = writeNoiseFrame(w, nil)
	_ = w.Flush()
	if _, err := readNoiseFrame(r, maxDataFrame); err != nil {
		t.Fatalf("server hello: %v", err)
	}

	inner := make([]byte, 4)
	binary.BigEndian.PutUint16(inner[0:2], uint16(msgListEntitiesReq))
	_ = writeNoiseFrame(w, inner)
	_ = w.Flush()

	if payload, err := readNoiseFrame(r, maxDataFrame); err == nil {
		if !bytes.Contains(payload, []byte(noiseMACFailure)) {
			t.Fatalf("an unauthenticated frame was answered with %d bytes", len(payload))
		}
	}
}

func TestAHandshakeWithAWrongPreambleIsRefused(t *testing.T) {
	psk := testPSK(t)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeNN,
		Initiator:             true,
		Prologue:              noisePrologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
		Random:                rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	s := testServer(t, psk)
	s.handshakeWait = time.Second
	near := serveOne(t, s)
	_ = near.SetDeadline(time.Now().Add(testTimeout))
	w := bufio.NewWriter(near)
	r := bufio.NewReader(near)
	_ = writeNoiseFrame(w, nil)
	_ = w.Flush()
	if _, err := readNoiseFrame(r, maxDataFrame); err != nil {
		t.Fatalf("server hello: %v", err)
	}

	_ = writeNoiseFrame(w, append([]byte{0x01}, msg1...))
	_ = w.Flush()
	if _, err := readNoiseFrame(r, maxDataFrame); err == nil {
		t.Fatal("a handshake with a non-zero preamble was answered")
	}
}

func TestARefusedConnectionDoesNotWaitForAWriterItNeverStarted(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.handshakeWait = 200 * time.Millisecond
	near, far := net.Pipe()
	served := make(chan struct{})
	go func() {
		s.serveConn(far)
		close(served)
	}()
	defer near.Close()

	w := bufio.NewWriter(near)
	_ = writeNoiseFrame(w, []byte{0x42})
	_ = w.Flush()

	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("a refused connection waited on a writer goroutine that was never started")
	}
}

func TestPlaintextClientIsAnsweredAndRefused(t *testing.T) {
	psk := make([]byte, noisePSKLen)
	if _, err := rand.Read(psk); err != nil {
		t.Fatal(err)
	}
	near, far := net.Pipe()
	served := make(chan struct{})
	go func() {
		testServer(t, psk).serveConn(far)
		close(served)
	}()
	defer func() {
		near.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("serveConn was still running after its client closed")
		}
	}()
	_ = near.SetDeadline(time.Now().Add(testTimeout))

	if _, err := near.Write([]byte{0x00, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	frame, err := readNoiseFrame(bufio.NewReader(near), maxDataFrame)
	if err != nil {
		t.Fatalf("plaintext client got no answer: %v", err)
	}
	if len(frame) != 0 {
		t.Errorf("answered with %d bytes, want an empty encrypted frame", len(frame))
	}
}

func TestTheWireConstantsAreWhatESPHomeExpects(t *testing.T) {
	if got := string(noisePrologue); got != "NoiseAPIInit\x00\x00" {
		t.Errorf("noisePrologue = %q", got)
	}
	if noiseCipherName != "Noise_NNpsk0_25519_ChaChaPoly_SHA256" {
		t.Errorf("noiseCipherName = %q", noiseCipherName)
	}
	built := "Noise_" + noise.HandshakeNN.Name + "psk0_" + string(noiseSuite.Name())
	if built != noiseCipherName {
		t.Errorf("the configured suite gives %q, want %q", built, noiseCipherName)
	}
}

func TestAReplayedHandshakeDoesNotBuyTheGrace(t *testing.T) {
	psk := make([]byte, noisePSKLen)
	if _, err := rand.Read(psk); err != nil {
		t.Fatal(err)
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256),
		Pattern:               noise.HandshakeNN,
		Initiator:             true,
		Prologue:              noisePrologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
		Random:                rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg1, _, _, err := hs.WriteMessage([]byte{0x00}, nil)
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", psk)
	s.handshakeWait = 600 * time.Millisecond
	near, far := net.Pipe()
	served := make(chan struct{})
	go func() {
		s.serveConn(far)
		close(served)
	}()
	defer func() {
		near.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("serveConn was still running after its client closed")
		}
	}()
	_ = near.SetDeadline(time.Now().Add(10 * time.Second))

	opened := time.Now()
	w := bufio.NewWriter(near)
	r := bufio.NewReader(near)
	_ = writeNoiseFrame(w, nil)
	_ = w.Flush()
	if _, err := readNoiseFrame(r, maxDataFrame); err != nil {
		t.Fatalf("server hello: %v", err)
	}

	time.Sleep(4 * s.handshakeWait / 5)
	_ = writeNoiseFrame(w, msg1)
	_ = w.Flush()
	if _, err := readNoiseFrame(r, maxDataFrame); err != nil {
		t.Fatalf("the replay was refused at the handshake: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := near.Read(make([]byte, 1)); done <- err }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a replayed handshake held its slot past the handshake wait; " +
			"eight of those lock Home Assistant out for the whole idle budget apiece")
	}
	if held := time.Since(opened); held > 3*s.handshakeWait/2 {
		t.Errorf("the slot was held %v, past the %v a peer that has proved nothing gets",
			held, s.handshakeWait)
	}
}

func TestAnOversizeHandshakeFrameIsRefusedForItsSize(t *testing.T) {
	for _, afterHello := range []bool{false, true} {
		name := "client hello"
		if afterHello {
			name = "handshake message"
		}
		t.Run(name, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			s := testServer(t, testPSK(t))
			s.handshakeWait = time.Second
			near, far := net.Pipe()
			served := make(chan struct{})
			go func() {
				s.serveConn(far)
				close(served)
			}()
			_ = near.SetDeadline(time.Now().Add(testTimeout))
			w := bufio.NewWriter(near)
			r := bufio.NewReader(near)

			if afterHello {
				if err := writeNoiseFrame(w, nil); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				if _, err := readNoiseFrame(r, maxDataFrame); err != nil {
					t.Fatalf("server hello: %v", err)
				}
			}

			if err := writeNoiseFrame(w, make([]byte, maxHandshakeFrame+1)); err != nil {
				t.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if _, err := readNoiseFrame(r, maxDataFrame); err == nil {
				t.Fatal("an oversize handshake frame was answered rather than refused")
			}

			near.Close()
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatal("serveConn was still running after its client closed")
			}
			want := fmt.Sprintf("exceeds the %d byte limit", maxHandshakeFrame)
			if got := out.String(); !strings.Contains(got, want) {
				t.Errorf("refused for the wrong reason, so nothing bounds what it allocates: %s", got)
			}
		})
	}
}

type deadSocket struct {
	net.Conn
	fail   chan struct{}
	once   sync.Once
	closed chan struct{}
}

func (d *deadSocket) Write(p []byte) (int, error) {
	select {
	case <-d.fail:
		return 0, errors.New("window shut")
	default:
		return d.Conn.Write(p)
	}
}

func (d *deadSocket) Close() error {
	d.once.Do(func() { close(d.closed) })
	return d.Conn.Close()
}

func TestAFailedWriteClosesTheSocketRatherThanWaitingOutTheIdleDeadline(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	near, far := net.Pipe()
	dead := &deadSocket{Conn: far, fail: make(chan struct{}), closed: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		s.serveConn(dead)
		close(served)
	}()
	defer near.Close()

	c, err := handshakeOver(t, near, psk)
	if err != nil {
		t.Fatal(err)
	}
	close(dead.fail)

	if err := c.send(msgListEntitiesReq, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dead.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("a failed write left the socket open, so the read loop holds the slot for the whole idle budget")
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Error("serveConn did not return after its socket was closed")
	}
}
