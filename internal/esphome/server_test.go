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

// handle reports a DisconnectRequest as an error, and the read loop is what acts
// on it. Covering the error being produced is not the same as covering it being
// obeyed: a loop that ignored it would answer the goodbye and then hold the
// connection open until the idle deadline.
func TestTheReadLoopActsOnHandlesError(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgDisconnectRequest, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("the goodbye was not answered: %v", err)
	}

	// Decided by the read returning rather than by a deadline: the connection has
	// 90 seconds of idle left, so only the teardown can end it this quickly.
	done := make(chan error, 1)
	go func() {
		_, _, err := c.recv()
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("the connection outlived the DisconnectRequest, so handle's error was ignored")
	}
}

func TestDisconnectIsAnsweredBeforeTheSocketCloses(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.send(msgDisconnectRequest, nil); err != nil {
		t.Fatal(err)
	}

	// net.Pipe delivers nothing until someone reads, so not reading yet leaves the
	// writer parked mid-write while serveConn is tearing the connection down. That
	// is the interleaving that matters: closing the socket there turns the goodbye
	// into an EOF, and Home Assistant logs a failed disconnect on every reconnect.
	time.Sleep(200 * time.Millisecond)

	msgType, _, err := c.recv()
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
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

func TestAClientThatHasSentAFrameKeepsItsSlot(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	s.handshakeWait = 200 * time.Millisecond
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	// One frame that decrypts, which is what a replayed handshake cannot manage.
	// Home Assistant sends a HelloRequest the moment the handshake is done.
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatal(err)
	}

	// Past the handshake wait. A client that has proved it has the key is allowed
	// to go quiet: Home Assistant holds the connection open between pings.
	time.Sleep(3 * s.handshakeWait)

	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatalf("ping after going quiet: %v", err)
	}
	msgType, _, err := c.recv()
	if err != nil {
		t.Fatalf("ping after going quiet: %v", err)
	}
	if msgType != msgPingResponse {
		t.Fatalf("answered with %d, want %d", msgType, msgPingResponse)
	}
}

// Bytes are not a handshake. Eight peers dribbling a frame each would otherwise
// hold every slot without ever proving they have the key.
func TestAnUnfinishedHandshakeDoesNotBuyTheGrace(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	s.handshakeWait = 200 * time.Millisecond
	client := serveOne(t, s)
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	// The client hello, and then nothing: the handshake is begun and never
	// finished.
	w := bufio.NewWriter(client)
	if err := writeNoiseFrame(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := readNoiseFrame(bufio.NewReader(client), maxDataFrame); err != nil {
		t.Fatalf("the server hello did not arrive: %v", err)
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
		t.Error("a half-finished handshake kept its slot past the handshake wait")
	}
}

// The log is a file on /data, and every byte below arrives from a peer that has
// the key. %q renders each of these bytes as four characters, so a full frame of
// client_info would quote to four times the frame.
func TestALongClientNameIsCutBeforeItReachesTheLog(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	var p pb
	p.str(1, strings.Repeat("\xff", maxNoiseMessage-16))
	if err := c.send(msgHelloRequest, p.b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("hello was not answered: %v", err)
	}

	// The reply is queued by handle, but the log line is written after handle
	// returns, so the reply arriving proves nothing about the log. A second round
	// trip does: the read loop writes the hello line before it reads again.
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("ping was not answered: %v", err)
	}

	// Bounded, rather than proportional to what arrived: the ceiling is the
	// truncation, not the frame.
	if n := len(out.String()); n > 1024 {
		t.Errorf("one hello wrote %d bytes of log for a %d byte name", n, maxNoiseMessage-16)
	}
}

// The server lock gates the accept path's cap check and every other
// connection's handler, so a read of procfs underneath it stalls all of them.
// The first sensor reading goes out from the read loop for that reason.
func TestTheUptimeReadDoesNotHoldTheServerLock(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	// Set on serveConn's goroutine and read after the reply has arrived, which
	// orders the two.
	locked := false
	s.uptime = func() (float32, bool) {
		if s.mu.TryLock() {
			s.mu.Unlock()
		} else {
			locked = true
		}
		return 1, true
	}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("the first reading did not arrive: %v", err)
	}

	if locked {
		t.Error("/proc/uptime was read with the server lock held")
	}
}

func TestTheLogRateLimitCapsWhatOnePeerCanWrite(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

	psk := testPSK(t)
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", psk)

	// Three ways to churn, because they reach different lines. Closing before
	// the lead byte never leaves the accept path; an empty hello and then a
	// close reaches "handshake failed", which needs no key at all. Both are a
	// peer's to repeat for as long as it likes.
	for i := 0; i < 200; i++ {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() { s.serveConn(server); close(done) }()
		if i%2 == 1 {
			w := bufio.NewWriter(client)
			_ = writeNoiseFrame(w, nil)
			_ = w.Flush()
			_, _ = readNoiseFrame(bufio.NewReader(client), maxDataFrame)
		}
		client.Close()
		<-done
	}
	if lines := strings.Count(out.String(), "\n"); lines > 2*logBurst {
		t.Errorf("200 connect/disconnect cycles wrote %d log lines, want at most %d",
			lines, 2*logBurst)
	}

	// The third is a session that succeeds, which needs the key and so is not a
	// stranger's to repeat. It still goes through the same limit, and the burst
	// above is spent: the line has to be suppressed rather than written.
	before := out.String()
	if _, err := dial(t, s, psk); err != nil {
		t.Fatal(err)
	}
	added := strings.TrimPrefix(out.String(), before)
	if strings.Contains(added, "encrypted session established") {
		t.Errorf("an established session wrote past a spent burst: %s", added)
	}
}

type fakeAddr struct{ net.Conn }

func (fakeAddr) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1234} }

// The three numbers are a chain rather than three independent choices: a push
// cancels the client's ping, so the deadline has to outlast a push interval plus
// the wait that follows it.
func TestTheIdleDeadlineOutlastsWhatTheClientSaysOnItsOwn(t *testing.T) {
	if MinSensorTick <= clientKeepalive {
		t.Errorf("a %v push interval cancels a %v keepalive before it ever fires, so the client falls silent",
			MinSensorTick, clientKeepalive)
	}
	if idleWait <= MinSensorTick+clientKeepalive {
		t.Errorf("idleWait is %v, and a client can legitimately say nothing for %v",
			idleWait, MinSensorTick+clientKeepalive)
	}
}

// The constant is exported as a contract, so the package has to hold it rather
// than leave it to a test on the caller: a second caller, or a flag, would get
// no protection from a test that compares two constants.
func TestPollSensorsWillNotAcceptATickThatSilencesTheClient(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	done := make(chan struct{})
	go func() { s.PollSensors(time.Millisecond); close(done) }()

	// It raises the tick to the floor rather than refusing, so the goroutine
	// stays alive; what is observable is the line saying so.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "raised to") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a 1ms tick was accepted; the log says %q", out.String())
}
