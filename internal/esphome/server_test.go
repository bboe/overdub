package esphome

import (
	"bufio"
	"bytes"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSendDropsRatherThanBlockingOnAStalledClient(t *testing.T) {
	s := &Server{}
	c := &conn{out: make(chan frame, 2)}
	for i := 0; i < cap(c.out); i++ {
		if err := s.send(c, msgPingResponse, nil); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- s.send(c, msgPingResponse, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("send accepted a frame with no room; a stalled client would grow without bound")
		}
	case <-time.After(5 * time.Second):
		// send runs under Server.mu. Blocking here parks every press, state and
		// command behind one client that stopped reading.
		t.Fatal("send blocked on a full queue")
	}
}

func TestDisconnectIsAnsweredBeforeTheSocketCloses(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	client, server := net.Pipe()
	defer client.Close()
	go s.serveConn(server)

	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(client)
	if err := writeFrame(w, msgDisconnectRequest, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	// net.Pipe delivers nothing until someone reads, so not reading yet leaves the
	// writer parked mid-write while serveConn is tearing the connection down. That
	// is the interleaving that matters: closing the socket there turns the goodbye
	// into an EOF, and Home Assistant logs a failed disconnect on every reconnect.
	time.Sleep(200 * time.Millisecond)

	msgType, _, err := readFrame(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("reading the reply to DisconnectRequest: %v", err)
	}
	if msgType != msgDisconnectResp {
		t.Errorf("replied with message %d, want %d", msgType, msgDisconnectResp)
	}
}

// serveOne wires a client end to a live serveConn, the way Listen does.
// Joined at cleanup, not just closed: a serveConn still running when its test
// ends writes its parting lines into the next test's captured log, and the log
// budgets here are exactly what that corrupts.
func serveOne(t *testing.T, s *Server) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		s.serveConn(server)
		close(done)
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serveConn was still running after its client closed")
		}
	})
	return client
}

func TestTheNinthConnectionIsRefused(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	for i := 0; i < maxConns; i++ {
		serveOne(t, s)
	}
	// Give the admissions above time to land in s.conns.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n == maxConns {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d connections were admitted", n, maxConns)
		}
		time.Sleep(5 * time.Millisecond)
	}

	over := serveOne(t, s)
	if err := over.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := over.Read(make([]byte, 1))
	if err == nil {
		t.Fatalf("connection %d was served; the cap does not hold", maxConns+1)
	}
	// A refused connection is closed at once. An admitted one just sits there, so
	// a read that times out means the cap let it in rather than turning it away.
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Errorf("connection %d was admitted and left open; the cap does not hold",
			maxConns+1)
	}
}

func TestAnUnsubscribedClientGetsNoStates(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	s.uptime = func() (float32, bool) { return 1234, true }
	quiet := &conn{out: make(chan frame, sendQueue)}
	loud := &conn{out: make(chan frame, sendQueue), states: true}
	s.mu.Lock()
	s.conns[quiet] = struct{}{}
	s.conns[loud] = struct{}{}
	s.mu.Unlock()

	s.pollOnce()

	if n := len(quiet.out); n != 0 {
		t.Errorf("a client that never subscribed was sent %d frames", n)
	}
	if len(loud.out) == 0 {
		t.Error("a subscribed client was sent nothing")
	}
}

func TestAClientThatSaysNothingLosesItsSlot(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	s.handshakeWait = 200 * time.Millisecond
	silent := serveOne(t, s)
	if err := silent.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := silent.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("a silent client was sent something")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Error("a client that never spoke kept its slot past the handshake wait; " +
			"eight of those lock Home Assistant out")
	}
}

func TestAClientThatHasSaidHelloKeepsItsSlot(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	s.handshakeWait = 200 * time.Millisecond
	client := serveOne(t, s)
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ping := func(when string) {
		t.Helper()
		w := bufio.NewWriter(client)
		if err := writeFrame(w, msgPingRequest, nil); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if err := w.Flush(); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		msgType, _, err := readFrame(bufio.NewReader(client))
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if msgType != msgPingResponse {
			t.Fatalf("%s: answered with %d, want %d", when, msgType, msgPingResponse)
		}
	}

	hello(t, client)
	// Past the handshake wait. A client that has introduced itself is allowed to
	// go quiet: Home Assistant holds the connection open between pings.
	time.Sleep(3 * s.handshakeWait)
	ping("ping after going quiet")
}

// A ping is not an introduction. Eight peers pinging once each would otherwise
// hold every slot for 90 seconds apiece and lock Home Assistant out.
func TestAPingDoesNotBuyTheHandshakeGrace(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	s.handshakeWait = 200 * time.Millisecond
	client := serveOne(t, s)
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	w := bufio.NewWriter(client)
	if err := writeFrame(w, msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrame(bufio.NewReader(client)); err != nil {
		t.Fatalf("ping was not answered: %v", err)
	}

	// Decided by the select below rather than by a read deadline: a deadline
	// expiring is indistinguishable from the server holding on, which is the
	// whole question here. The connection deadline set above is 10s, far past
	// this, so it cannot be what answers.
	done := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the connection was still open, and sent something")
		}
	case <-time.After(3 * time.Second):
		t.Error("a client that only pinged kept its slot past the handshake wait")
	}
}

func hello(t *testing.T, client net.Conn) {
	t.Helper()
	var p pb
	p.str(1, "test client")
	w := bufio.NewWriter(client)
	if err := writeFrame(w, msgHelloRequest, p.b); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	msgType, _, err := readFrame(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("hello was not answered: %v", err)
	}
	if msgType != msgHelloResponse {
		t.Fatalf("hello answered with %d, want %d", msgType, msgHelloResponse)
	}
}

// The log is a file on /data, and every byte below arrives from an
// unauthenticated peer, and %q renders each of these bytes as four characters,
// so a full frame of client_info quotes to four times maxFrame.
func TestALongClientNameIsCutBeforeItReachesTheLog(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	var p pb
	p.str(1, strings.Repeat("\xff", maxFrame-16))

	// Through serveConn, because the read loop is what writes the line: driving
	// handle alone only sets the field it carries out, and would pass with no
	// truncation at all.
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { s.serveConn(server); close(done) }()
	w := bufio.NewWriter(client)
	if err := writeFrame(w, msgHelloRequest, p.b); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrame(bufio.NewReader(client)); err != nil {
		t.Fatalf("hello was not answered: %v", err)
	}
	client.Close()
	<-done
	// Bounded, rather than proportional to what arrived: %q renders each of
	// these bytes as four characters, so the ceiling is the truncation, not
	// the frame.
	if n := len(out.String()); n > 1024 {
		t.Errorf("one hello wrote %d bytes of log for a %d byte name", n, maxFrame-16)
	}
}

// The server lock gates the accept path's cap check and every other
// connection's handler, so a read of procfs underneath it stalls all of them.
// The first sensor reading goes out from the read loop for that reason.
func TestTheUptimeReadDoesNotHoldTheServerLock(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	// Set on serveConn's goroutine and read after it has returned, so the receive
	// below orders the two.
	locked := false
	s.uptime = func() (float32, bool) {
		if s.mu.TryLock() {
			s.mu.Unlock()
		} else {
			locked = true
		}
		return 1, true
	}

	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { s.serveConn(server); close(done) }()
	w := bufio.NewWriter(client)
	if err := writeFrame(w, msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrame(bufio.NewReader(client)); err != nil {
		t.Fatalf("the first reading did not arrive: %v", err)
	}
	client.Close()
	<-done

	if locked {
		t.Error("/proc/uptime was read with the server lock held")
	}
}

func TestTheLogRateLimitCapsWhatOnePeerCanWrite(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	for i := 0; i < 500; i++ {
		s.peerLogf("esphome api: line %d", i)
	}
	if lines := strings.Count(out.String(), "\n"); lines > logBurst+1 {
		t.Errorf("500 peer events wrote %d lines, want at most %d", lines, logBurst+1)
	}
}

// Every line the api writes is caused by a peer, and eight of them can be in
// handlers at once. What catches a missing lock here is the race detector rather
// than the count below, so this one earns its keep only under -race, which CI
// runs natively because GOARCH=arm has no detector.
func TestTheRateLimitHoldsAcrossConcurrentPeers(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	var wg sync.WaitGroup
	for i := 0; i < maxConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.peerLogf("esphome api: line %d", j)
			}
		}()
	}
	wg.Wait()

	if lines := strings.Count(out.String(), "\n"); lines > logBurst+1 {
		t.Errorf("%d peers wrote %d lines in one window, want at most %d",
			maxConns, lines, logBurst+1)
	}
}

// The rate limit alone bounds bytes per minute, not bytes: nothing truncates
// this log while the daemon runs, so a peer that keeps at it for a week would
// still fill /data.
func TestPeerLoggingStopsAtItsCeilingForTheRun(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	for i := 0; i < logTotal+logBurst*3; i++ {
		// Past the window each time, so the rate limit never refuses: the total
		// is what has to stop it.
		s.logMu.Lock()
		s.logWindowEnd = time.Time{}
		s.logMu.Unlock()
		s.peerLogf("esphome api: line %d", i)
	}
	if lines := strings.Count(out.String(), "\n"); lines > logTotal+2 {
		t.Errorf("wrote %d lines, want at most %d: the run has no ceiling",
			lines, logTotal+2)
	}
}

// Answering would mean replying to whatever was scraped out of the message
// before the parse went wrong.
func TestAMalformedHelloIsNotAnswered(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	conn := &conn{sock: fakeAddr{}, out: make(chan frame, sendQueue)}
	if err := s.handle(conn, msgHelloRequest, []byte{0x08}); err == nil {
		t.Error("a truncated HelloRequest was accepted")
	}
	if len(conn.out) != 0 {
		t.Errorf("a truncated HelloRequest was answered with %d frames", len(conn.out))
	}
}

// Goroutines left running by earlier tests still log, so the buffer is locked
// and the previous writer is put back rather than assumed to be stderr.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func restoreLog(t *testing.T, buf *lockedBuffer) func() {
	t.Helper()
	was := log.Writer()
	log.SetOutput(buf)
	return func() { log.SetOutput(was) }
}

// Every line the api writes is caused by a peer, so a peer that reconnects in a
// loop must not be able to write one apiece: the log is a file on /data.
func TestChurnCannotOutrunTheLogRateLimit(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A")
	for i := 0; i < 200; i++ {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() { s.serveConn(server); close(done) }()
		w := bufio.NewWriter(client)
		_ = writeFrame(w, msgDisconnectRequest, nil)
		_ = w.Flush()
		_, _, _ = readFrame(bufio.NewReader(client))
		client.Close()
		<-done
	}
	if lines := strings.Count(out.String(), "\n"); lines > 2*logBurst {
		t.Errorf("200 connect/disconnect cycles wrote %d log lines, want at most %d",
			lines, 2*logBurst)
	}
}

type fakeAddr struct{ net.Conn }

func (fakeAddr) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1234} }
