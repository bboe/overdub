package sendspin

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeConn struct {
	in  *bytes.Reader
	out bytes.Buffer
}

func (f *fakeConn) Read(b []byte) (int, error)       { return f.in.Read(b) }
func (f *fakeConn) Write(b []byte) (int, error)      { return f.out.Write(b) }
func (f *fakeConn) Close() error                     { return nil }
func (f *fakeConn) LocalAddr() net.Addr              { return nil }
func (f *fakeConn) RemoteAddr() net.Addr             { return nil }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func newTestConn(data []byte) (*Conn, *fakeConn) {
	f := &fakeConn{in: bytes.NewReader(data)}
	return &Conn{c: f, r: bufio.NewReader(f), limit: maxMessage}, f
}

var testMask = [4]byte{0xa1, 0xb2, 0xc3, 0xd4}

func frame(fin bool, op opcode, payload []byte) []byte {
	first := byte(op)
	if fin {
		first |= 0x80
	}
	var head []byte
	switch n := len(payload); {
	case n <= maxControlPayload:
		head = []byte{first, byte(n) | 0x80}
	case n <= 0xffff:
		head = []byte{first, 126 | 0x80, 0, 0}
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = make([]byte, 10)
		head[0], head[1] = first, 127|0x80
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}
	out := append(head, testMask[:]...)
	for i, b := range payload {
		out = append(out, b^testMask[i%4])
	}
	return out
}

func upgradeRequest(path, key, version, upgrade, connection string) string {
	return fmt.Sprintf("GET %s HTTP/1.1\r\nHost: dot\r\nUpgrade: %s\r\n"+
		"Connection: %s\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: %s\r\n\r\n",
		path, upgrade, connection, key, version)
}

const rfcKey = "dGhlIHNhbXBsZSBub25jZQ=="

const rfcAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="

func TestAcceptCompletesTheUpgrade(t *testing.T) {
	f := &fakeConn{in: bytes.NewReader([]byte(
		upgradeRequest("/sendspin", rfcKey, "13", "websocket", "Upgrade")))}
	if _, err := Accept(f, "/sendspin"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	got := f.out.String()
	if !strings.HasPrefix(got, "HTTP/1.1 101 Switching Protocols\r\n") {
		t.Errorf("response did not begin with 101:\n%s", got)
	}
	if !strings.Contains(got, "Sec-WebSocket-Accept: "+rfcAccept+"\r\n") {
		t.Errorf("accept key wrong; want %q in:\n%s", rfcAccept, got)
	}
}

func TestAcceptRefusesABadRequest(t *testing.T) {
	for _, c := range []struct {
		name    string
		request string
		status  string
	}{
		{"wrong path", upgradeRequest("/nope", rfcKey, "13", "websocket", "Upgrade"), "404"},
		{"old version", upgradeRequest("/sendspin", rfcKey, "8", "websocket", "Upgrade"), "426"},
		{"no upgrade", upgradeRequest("/sendspin", rfcKey, "13", "h2c", "Upgrade"), "400"},
		{"no connection", upgradeRequest("/sendspin", rfcKey, "13", "websocket", "keep-alive"), "400"},
		{"short key", upgradeRequest("/sendspin", "c2hvcnQ=", "13", "websocket", "Upgrade"), "400"},
		{"not base64", upgradeRequest("/sendspin", "!!!!", "13", "websocket", "Upgrade"), "400"},
		{"not a GET", "POST /sendspin HTTP/1.1\r\nHost: dot\r\nUpgrade: websocket\r\n" +
			"Connection: Upgrade\r\nSec-WebSocket-Key: " + rfcKey +
			"\r\nSec-WebSocket-Version: 13\r\n\r\n", "400"},
		{"not HTTP/1.1", "GET /sendspin HTTP/1.0\r\nHost: dot\r\nUpgrade: websocket\r\n" +
			"Connection: Upgrade\r\nSec-WebSocket-Key: " + rfcKey +
			"\r\nSec-WebSocket-Version: 13\r\n\r\n", "400"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeConn{in: bytes.NewReader([]byte(c.request))}
			if _, err := Accept(f, "/sendspin"); err == nil {
				t.Fatal("Accept succeeded on a request it should refuse")
			}
			if !strings.Contains(f.out.String(), c.status) {
				t.Errorf("want status %s, got:\n%s", c.status, f.out.String())
			}
		})
	}
}

func TestAcceptTakesAQueryStringOnThePath(t *testing.T) {
	f := &fakeConn{in: bytes.NewReader([]byte(
		upgradeRequest("/sendspin?v=1", rfcKey, "13", "websocket", "Upgrade")))}
	if _, err := Accept(f, "/sendspin"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func TestAcceptIsCaseInsensitiveOnHeaders(t *testing.T) {
	req := "GET /sendspin HTTP/1.1\r\nUPGRADE: WebSocket\r\n" +
		"connection: keep-alive, Upgrade\r\nSec-WebSocket-Key: " + rfcKey +
		"\r\nSec-WebSocket-Version: 13\r\n\r\n"
	f := &fakeConn{in: bytes.NewReader([]byte(req))}
	if _, err := Accept(f, "/sendspin"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func TestReadReturnsTextAndBinary(t *testing.T) {
	data := append(frame(true, opText, []byte(`{"type":"server/init"}`)),
		frame(true, opBinary, []byte{0xde, 0xad, 0xbe, 0xef})...)
	w, _ := newTestConn(data)

	isBinary, msg, err := w.Read()
	if err != nil {
		t.Fatalf("Read text: %v", err)
	}
	if isBinary {
		t.Error("a text frame read as binary")
	}
	if string(msg) != `{"type":"server/init"}` {
		t.Errorf("text payload = %q", msg)
	}

	isBinary, msg, err = w.Read()
	if err != nil {
		t.Fatalf("Read binary: %v", err)
	}
	if !isBinary {
		t.Error("a binary frame read as text")
	}
	if !bytes.Equal(msg, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("binary payload = %x", msg)
	}
}

func TestReadReassemblesFragments(t *testing.T) {
	data := frame(false, opBinary, []byte("one "))
	data = append(data, frame(false, opContinuation, []byte("two "))...)
	data = append(data, frame(true, opContinuation, []byte("three"))...)
	w, _ := newTestConn(data)
	isBinary, msg, err := w.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !isBinary {
		t.Error("fragmented binary read as text")
	}
	if string(msg) != "one two three" {
		t.Errorf("reassembled to %q", msg)
	}
}

func TestReadAnswersAPingAndCarriesOn(t *testing.T) {
	data := append(frame(true, opPing, []byte("hi")),
		frame(true, opText, []byte("after"))...)
	w, f := newTestConn(data)
	_, msg, err := w.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(msg) != "after" {
		t.Errorf("message = %q", msg)
	}
	out := f.out.Bytes()
	if len(out) < 2 || opcode(out[0]&0x0f) != opPong {
		t.Fatalf("no pong written; got %x", out)
	}
	if out[1]&0x80 != 0 {
		t.Error("a server frame was masked")
	}
	if string(out[2:]) != "hi" {
		t.Errorf("pong did not echo the ping payload: %q", out[2:])
	}
}

func TestReadSkipsAPong(t *testing.T) {
	data := append(frame(true, opPong, nil), frame(true, opText, []byte("x"))...)
	w, _ := newTestConn(data)
	if _, msg, err := w.Read(); err != nil || string(msg) != "x" {
		t.Fatalf("Read = %q, %v", msg, err)
	}
}

func TestReadReportsAClose(t *testing.T) {
	payload := []byte{0x03, 0xe8}
	payload = append(payload, "bye"...)
	w, _ := newTestConn(frame(true, opClose, payload))
	_, _, err := w.Read()
	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("want a CloseError, got %v", err)
	}
	if ce.Code != 1000 || ce.Reason != "bye" {
		t.Errorf("close = %d %q", ce.Code, ce.Reason)
	}
}

func TestReadReportsACloseWithNoStatus(t *testing.T) {
	w, _ := newTestConn(frame(true, opClose, nil))
	_, _, err := w.Read()
	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("want a CloseError, got %v", err)
	}
	if ce.Code != closeNoStatus {
		t.Errorf("close code = %d, want %d", ce.Code, closeNoStatus)
	}
}

func TestReadRefusesMalformedFrames(t *testing.T) {
	unmasked := []byte{0x81, 0x01, 'x'}
	reserved := frame(true, opText, []byte("x"))
	reserved[0] |= 0x40
	longControl := frame(true, opPing, bytes.Repeat([]byte("x"), 126))
	splitControl := frame(false, opPing, []byte("x"))
	nonMinimal16 := []byte{0x81, 126 | 0x80, 0x00, 0x05, 0, 0, 0, 0, 'h', 'e', 'l', 'l', 'o'}
	highBitLength := []byte{0x81, 127 | 0x80, 0xff, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0}
	nonMinimal64 := []byte{0x81, 127 | 0x80, 0, 0, 0, 0, 0, 0, 0x00, 0x64, 0, 0, 0, 0}
	oneByteClose := frame(true, opClose, []byte{0x03})

	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"unmasked", unmasked, errProtocol},
		{"reserved bit", reserved, errProtocol},
		{"oversize control", longControl, errProtocol},
		{"fragmented control", splitControl, errProtocol},
		{"oversize close", frame(true, opClose, bytes.Repeat([]byte("x"), 126)), errProtocol},
		{"fragmented close", frame(false, opClose, []byte{0x03, 0xe8}), errProtocol},
		{"oversize pong", frame(true, opPong, bytes.Repeat([]byte("x"), 126)), errProtocol},
		{"non-minimal 16-bit length", nonMinimal16, errProtocol},
		{"high bit in 64-bit length", highBitLength, errProtocol},
		{"non-minimal 64-bit length", nonMinimal64, errProtocol},
		{"one-byte close", oneByteClose, errProtocol},
		{"continuation with nothing in flight", frame(true, opContinuation, []byte("x")), errProtocol},
		{"unknown opcode", frame(true, opcode(0x3), []byte("x")), errProtocol},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := newTestConn(c.data)
			if _, _, err := w.Read(); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestReadRefusesANewMessageMidFragment(t *testing.T) {
	data := append(frame(false, opBinary, []byte("a")), frame(true, opText, []byte("b"))...)
	w, _ := newTestConn(data)
	if _, _, err := w.Read(); !errors.Is(err, errProtocol) {
		t.Errorf("err = %v, want %v", err, errProtocol)
	}
}

func TestReadRefusesAnOversizeFrame(t *testing.T) {
	w, _ := newTestConn(frame(true, opBinary, bytes.Repeat([]byte("x"), 200)))
	w.setReadLimit(100)
	if _, _, err := w.Read(); !errors.Is(err, errTooBig) {
		t.Errorf("err = %v, want %v", err, errTooBig)
	}
}

func TestReadRefusesFragmentsThatOutgrowTheLimit(t *testing.T) {
	data := frame(false, opBinary, bytes.Repeat([]byte("x"), 60))
	data = append(data, frame(true, opContinuation, bytes.Repeat([]byte("y"), 60))...)
	w, _ := newTestConn(data)
	w.setReadLimit(100)
	if _, _, err := w.Read(); !errors.Is(err, errTooBig) {
		t.Errorf("err = %v, want %v", err, errTooBig)
	}
}

func TestWriteFramesAreUnmaskedAndSized(t *testing.T) {
	for _, c := range []struct {
		name    string
		n       int
		wantLen byte
		header  int
	}{
		{"short", 5, 5, 2},
		{"16-bit", 200, 126, 4},
		{"64-bit", 70000, 127, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, f := newTestConn(nil)
			if err := w.WriteBinary(bytes.Repeat([]byte("x"), c.n)); err != nil {
				t.Fatalf("WriteBinary: %v", err)
			}
			out := f.out.Bytes()
			if out[0] != byte(opBinary)|0x80 {
				t.Errorf("first byte = %#x", out[0])
			}
			if out[1] != c.wantLen {
				t.Errorf("length byte = %d, want %d", out[1], c.wantLen)
			}
			switch c.wantLen {
			case 126:
				if got := int(binary.BigEndian.Uint16(out[2:4])); got != c.n {
					t.Errorf("16-bit length field = %d, want %d", got, c.n)
				}
			case 127:
				if got := int(binary.BigEndian.Uint64(out[2:10])); got != c.n {
					t.Errorf("64-bit length field = %d, want %d", got, c.n)
				}
			}
			if out[1]&0x80 != 0 {
				t.Error("a server frame was masked")
			}
			if len(out) != c.header+c.n {
				t.Errorf("wrote %d bytes, want %d", len(out), c.header+c.n)
			}
		})
	}
}

func FuzzFrameReader(f *testing.F) {
	f.Add(frame(true, opText, []byte("hello")))
	f.Add(frame(true, opBinary, []byte{0, 1, 2}))
	f.Add(frame(true, opPing, []byte("p")))
	f.Add(frame(true, opClose, []byte{0x03, 0xe8}))
	f.Add(append(frame(false, opBinary, []byte("a")),
		frame(true, opContinuation, []byte("b"))...))
	f.Add([]byte{0x81, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		w, _ := newTestConn(data)
		w.setReadLimit(4096)
		for range 8 {
			_, msg, err := w.Read()
			if err != nil {
				return
			}
			if len(msg) > 4096 {
				t.Fatalf("Read returned %d bytes past the 4096 limit", len(msg))
			}
		}
	})
}

func TestAMessageSplitAcrossTooManyFramesIsRefused(t *testing.T) {
	data := frame(false, opBinary, nil)
	// A literal rather than maxWSFrames+10: a loop bound read from the constant under
	// test runs forever when that constant is what moved, and a test that hangs on a
	// regression burns CI's job limit and reports nothing.
	for i := 0; i < 4106; i++ {
		data = append(data, frame(false, opContinuation, nil)...)
	}
	w, _ := newTestConn(data)

	done := make(chan error, 1)
	go func() { _, _, err := w.Read(); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errTooMany) {
			t.Errorf("Read() = %v, want %v: a fragment chain bounded only in bytes lets a peer"+
				" stay inside one Read for as long as it keeps sending headers", err, errTooMany)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Read never returned; an endless fragment chain holds the connection past every" +
			" deadline, because each header re-arms the idle read deadline")
	}
}

func TestAnEmptyFragmentChainDoesNotCountAgainstTheByteLimit(t *testing.T) {
	data := frame(false, opBinary, []byte("a"))
	for i := 0; i < 8; i++ {
		data = append(data, frame(false, opContinuation, nil)...)
	}
	data = append(data, frame(true, opContinuation, []byte("b"))...)
	w, _ := newTestConn(data)
	_, msg, err := w.Read()
	if err != nil {
		t.Fatalf("a short chain of empty fragments was refused: %v", err)
	}
	if string(msg) != "ab" {
		t.Errorf("reassembled %q, want %q", msg, "ab")
	}
}

// A connection deadline set before activation bounds writes as well as reads.
// Leaving it in place made every write fail once the provisional window passed,
// which on a real server looked like a reconnect every thirty seconds.
func TestWriteRefreshesItsOwnDeadline(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if err := c1.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	w := &Conn{c: c1, r: bufio.NewReader(c1), limit: maxMessage}
	w.setIdle(2 * time.Second)

	go func() { io.Copy(io.Discard, c2) }()
	if err := w.WriteText([]byte("after the provisional window")); err != nil {
		t.Fatalf("write failed with a stale deadline in place: %v", err)
	}
}

func TestWriteIsStillBoundedWhenIdleIsSet(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	w := &Conn{c: c1, r: bufio.NewReader(c1), limit: maxMessage}
	w.setIdle(100 * time.Millisecond)

	// Nothing reads c2, so net.Pipe blocks until the write deadline fires. The
	// result arrives on a channel because the bug this pins is a write that never
	// returns: read it inline and a regression hangs the package until the test
	// binary's own timeout, which in CI is the job limit and no useful output.
	done := make(chan error, 1)
	go func() { done <- w.WriteText([]byte("nobody is reading")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a write to a peer that never reads did not time out")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write to a peer that never reads never returned, so nothing bounds it")
	}
}

func TestReadRefusesALengthTooBigToAllocate(t *testing.T) {
	head := []byte{0x82, 127 | 0x80, 0x40, 0, 0, 0, 0, 0, 0, 0}
	w, _ := newTestConn(append(head, testMask[:]...))
	if _, _, err := w.Read(); !errors.Is(err, errTooBig) {
		t.Errorf("err = %v, want %v", err, errTooBig)
	}
}

func TestAcceptRefusesTooManyHeaderLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("GET /sendspin HTTP/1.1\r\n")
	for i := 0; i < maxRequestLines+1; i++ {
		fmt.Fprintf(&b, "X-Pad-%d: x\r\n", i)
	}
	b.WriteString("\r\n")
	f := &fakeConn{in: bytes.NewReader([]byte(b.String()))}
	if _, err := Accept(f, "/sendspin"); err == nil {
		t.Fatal("Accept read an unbounded number of headers")
	}
	if !strings.Contains(f.out.String(), "431") {
		t.Errorf("want status 431, got:\n%s", f.out.String())
	}
}

func TestAcceptRefusesAHeaderLineWithNoEnd(t *testing.T) {
	data := append([]byte("GET /sendspin HTTP/1.1\r\nX-Pad: "),
		bytes.Repeat([]byte("x"), maxRequestBytes+1)...)
	f := &fakeConn{in: bytes.NewReader(data)}
	if _, err := Accept(f, "/sendspin"); err == nil {
		t.Fatal("Accept grew one header line without bound")
	}
	if !strings.Contains(f.out.String(), "431") {
		t.Errorf("want status 431, got:\n%s", f.out.String())
	}
}

func TestTheWireValuesRFC6455Fixes(t *testing.T) {
	// Named against literals rather than against each other: a test that says
	// opPing == opPing holds while the byte on the wire changes under it.
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"continuation", int(opContinuation), 0x0},
		{"text", int(opText), 0x1},
		{"binary", int(opBinary), 0x2},
		{"close", int(opClose), 0x8},
		{"ping", int(opPing), 0x9},
		{"pong", int(opPong), 0xa},
		{"no status close", int(closeNoStatus), 1005},
		{"control payload cap", maxControlPayload, 125},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
	if wsAcceptSalt != "258EAFA5-E914-47DA-95CA-C5AB0DC85B11" {
		t.Errorf("the RFC 6455 accept salt is %q", wsAcceptSalt)
	}
}

func TestPingIsSentAsAControlFrame(t *testing.T) {
	// Nothing else drives Ping, so neither the opcode it uses nor the fact that the
	// keepalive reaches the socket at all is otherwise checked.
	w, f := newTestConn(nil)
	if err := w.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	out := f.out.Bytes()
	if len(out) != 2 {
		t.Fatalf("ping wrote %d bytes, want a 2-byte header and no payload: %#v", len(out), out)
	}
	if out[0] != byte(opPing)|0x80 {
		t.Errorf("first byte = %#x, want %#x", out[0], byte(opPing)|0x80)
	}
	if out[1] != 0 {
		t.Errorf("length byte = %d, want 0", out[1])
	}
}

func TestConcurrentWritesDoNotInterleaveFrames(t *testing.T) {
	// keepalive pings from one goroutine while a session writes from another, which is
	// what production does, over a real socket with 64 KB payloads the kernel has to
	// split.
	//
	// What keeps the frames whole is that each one is a single Write and Go holds a
	// per-connection write lock, so this passes with Conn.mu removed -- measured. The
	// mutex's real job is the SetWriteDeadline-then-Write pair, not the framing. This
	// asserts the property a peer depends on; it does not pin the mutex.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	type accepted struct {
		c   net.Conn
		err error
	}
	acc := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		acc <- accepted{c, err}
	}()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c1.Close()
	got := <-acc
	if got.err != nil {
		t.Fatalf("Accept: %v", got.err)
	}
	c2 := got.c
	defer c2.Close()
	w := &Conn{c: c1, r: bufio.NewReader(c1), limit: maxMessage}

	const writes = 40
	body := bytes.Repeat([]byte("abcdefgh"), 8192) // 64 KB, past any socket buffer
	go func() {
		var wg sync.WaitGroup
		for range writes {
			wg.Add(2)
			go func() { defer wg.Done(); _ = w.WriteBinary(body) }()
			go func() { defer wg.Done(); _ = w.Ping() }()
		}
		wg.Wait()
		c1.Close()
	}()

	// Read every frame back off the wire and check each is the shape it should be.
	r := bufio.NewReader(c2)
	binaries := 0
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			break
		}
		op := opcode(h[0] & 0x0f)
		n := int(h[1] & 0x7f)
		switch n {
		case 126:
			var e [2]byte
			if _, err := io.ReadFull(r, e[:]); err != nil {
				t.Fatal("truncated 16-bit length")
			}
			n = int(binary.BigEndian.Uint16(e[:]))
		case 127:
			var e [8]byte
			if _, err := io.ReadFull(r, e[:]); err != nil {
				t.Fatal("truncated 64-bit length")
			}
			n = int(binary.BigEndian.Uint64(e[:]))
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			break
		}
		switch op {
		case opBinary:
			if !bytes.Equal(payload, body) {
				t.Fatalf("a binary frame carried %d bytes of something else", len(payload))
			}
			binaries++
		case opPing:
			if n != 0 {
				t.Fatalf("a ping carried %d bytes", n)
			}
		default:
			t.Fatalf("read opcode %#x off the wire", op)
		}
	}
	if binaries != writes {
		t.Errorf("read %d whole binary frames, want %d", binaries, writes)
	}
}

func TestReadGivesUpOnAnIdlePeer(t *testing.T) {
	// The idle window is what drops a holder whose server lost power. Nothing else
	// asserts that setting it has any effect.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	w := &Conn{c: c1, r: bufio.NewReader(c1), limit: maxMessage}
	w.setIdle(100 * time.Millisecond)

	done := make(chan error, 1)
	go func() { _, _, err := w.Read(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a read against a silent peer returned without error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read against a silent peer never returned, so the idle window does nothing")
	}
}
