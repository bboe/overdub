package esphome

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"math"
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
	// a full idle budget left, so only the teardown can end it this quickly.
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

	s.publish("sensors", s.readTicked())

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

// Two rules at once, and the second is what the published state rests on: the
// pollers are the only readers of the device, and they read outside the lock.
// A reading taken on a connection's goroutine would be a second reader, and one
// taken under the lock would stall the accept path and every other connection,
// since handle holds that lock for its whole body.
//
// Driven through PollSensors rather than through a helper that calls publish
// the way it does. A helper cannot say where the real poll takes the lock, and
// a suite that only asks the helper stays green with the read moved inside it.
func TestOnlyThePollersReadTheDeviceAndNeverUnderTheLock(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)

	var mu sync.Mutex
	reads, underLock := 0, false
	read := make(chan struct{}, 1)
	watch := func(v float32) func() (float32, bool) {
		return func() (float32, bool) {
			mu.Lock()
			reads++
			// Retried rather than asked once. TryLock reports whether anybody
			// holds the lock, not whether this goroutine does, and a handler
			// holding it for its own body would otherwise read as this poll
			// holding it. A caller that really holds it fails every attempt,
			// because the lock is not reentrant; a passing handler does not.
			locked := false
			for i := 0; i < 200; i++ {
				if s.mu.TryLock() {
					s.mu.Unlock()
					locked = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !locked {
				underLock = true
			}
			select {
			case read <- struct{}{}:
			default:
			}
			mu.Unlock()
			return v, true
		}
	}
	s.uptime, s.wifi, s.volume = watch(1), watch(-48), watch(40)

	// Ticks far enough away that every read below is either the sensor poll's
	// startup publish or one a subscriber woke.
	go s.Poll(MinSensorTick, time.Hour)
	<-read

	mu.Lock()
	polled := reads
	if underLock {
		t.Error("the poll read the device with the server lock held")
	}
	mu.Unlock()
	if polled == 0 {
		t.Fatal("the poll read nothing, so this test would pass on a server that never reads")
	}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < sensorCount; n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	// Subscribing wakes both polls, so the reads it is allowed are the woken
	// ones: what must not happen is a reading taken on the connection's own
	// goroutine, which is what the lock check above would catch and what the
	// count below bounds. polled is one sensor poll's worth, since only the
	// startup publish had run when it was taken, so the ceiling is that again
	// plus the one volume read the wake buys. Calling readTicked here instead
	// would deadlock: the stubs take this same lock.
	mu.Lock()
	defer mu.Unlock()
	if underLock {
		t.Error("the device was read with the server lock held")
	}
	if reads > 2*polled+1 {
		t.Errorf("answering a subscriber took %d readings beyond the wake's own; the snapshot has to replay what was published, or the two readers can disagree",
			reads-2*polled-1)
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

// A client that is still talking fills the deadline by itself, and the ping is
// for one that has stopped. aioesphomeapi's timer is fixed and repeating rather
// than reset by traffic, so a message buys one tick and the longest a healthy
// client stays quiet is two of them. Ping sooner than that and every connection
// is asked something it was already about to say.
func TestAClientThatIsStillTalkingIsNeverPinged(t *testing.T) {
	if quiet := 2 * clientKeepalive; pingAfter <= quiet {
		t.Errorf("pingAfter is %v, but a client that is talking normally can be quiet for %v",
			pingAfter, quiet)
	}
}

// The constant is exported as a contract, so the package has to hold it rather
// than leave it to a test on the caller: a second caller, or a flag, would get
// no protection from a test that compares two constants.
func TestPollSensorsWillNotAcceptATickUnderTheFloor(t *testing.T) {
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

// Every sensor reading is a key and a float, and the key is what tells Home
// Assistant which entity it belongs to. Two readings go out per tick, so a
// wrong key does not lose the value: it files it under the other sensor.
func sensorReading(t *testing.T, payload []byte) (uint32, float32, bool) {
	t.Helper()
	var key uint32
	var value float32
	missing := false
	seen := map[int]int{}
	if err := pbWalk(payload, func(f pbField) {
		seen[f.field] = f.wire
		switch f.field {
		case 1:
			key = uint32(f.num)
		case 2:
			value = math.Float32frombits(uint32(f.num))
		case 3:
			missing = f.num != 0
		}
	}); err != nil {
		t.Fatalf("sensor state did not parse: %v", err)
	}
	// The key and the value are fixed32 on the wire. Sent as varints they decode
	// to the same number here and to nothing at all in Home Assistant, which
	// skips the field it cannot read and files every reading under key zero.
	if seen[1] != wireFixed32 {
		t.Errorf("the key went out as wire type %d, want fixed32 (%d)", seen[1], wireFixed32)
	}
	if seen[2] != wireFixed32 {
		t.Errorf("the value went out as wire type %d, want fixed32 (%d)", seen[2], wireFixed32)
	}
	return key, value, missing
}

// Every sensor the server pushes, stubbed to a value nothing else would
// produce, so a reading filed under the wrong key is visible rather than
// plausible. A sensor added later is a line here and a line in want().
func stubSensors(s *Server) map[uint32]float32 {
	s.uptime = func() (float32, bool) { return 1234, true }
	s.wifi = func() (float32, bool) { return -48, true }
	s.volume = func() (float32, bool) { return 40, true }
	s.cpu = func() (float32, bool) { return 41.3, true }
	s.memory = func() (float32, bool) { return 126.5, true }
	return map[uint32]float32{
		s.keyUptime: 1234, s.keyWifi: -48, s.keyVolume: 40, s.keyCPU: 41.3, s.keyMemory: 126.5,
	}
}

// How many readings a subscriber's snapshot carries once everything has been
// polled. A constant rather than a call: the tests that count device reads stub
// the readers, so anything that asks the server how many sensors it has would
// be counted as a read of its own. TestTheSensorCountMatchesTheListing keeps it
// honest.
const sensorCount = 5

// What the two pollers put into the published state, without their tickers.
// Returns what they changed, for the tests that care.
func pollAll(s *Server) []reading {
	changed := s.publish("sensors", s.readTicked())
	return append(changed, s.publish("live", s.readLive())...)
}

func TestSubscribingGetsEverySensor(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)
	pollAll(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}

	// Counted rather than ranged: deleting from a map inside a range over it
	// may end the loop early, which read one message instead of two and failed
	// two runs in five.
	for n := len(want); n > 0; n-- {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("a reading did not arrive: %v", err)
		}
		if msgType != msgSensorState {
			t.Fatalf("got message type %d, want %d", msgType, msgSensorState)
		}
		key, value, missing := sensorReading(t, payload)
		expected, known := want[key]
		if !known {
			t.Fatalf("a reading arrived under key %d, which is no sensor of ours", key)
		}
		if value != expected {
			t.Errorf("key %d carried %v, want %v", key, value, expected)
		}
		if missing {
			t.Errorf("key %d was marked missing, so Home Assistant shows no value for a reading that succeeded", key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("%d sensor readings never arrived", len(want))
	}
}

// A reading that could not be taken is sent and flagged, not sent as zero and
// not left out. Zero is a plausible value for all three, and Home Assistant
// would draw it as a measurement; leaving it out is no better once a value has
// been published, because the old one stays on screen as though it were current.
// missing_state is the field the protocol has for exactly this.
func TestAReadingThatFailedIsSentAsMissing(t *testing.T) {
	for _, failing := range []string{"uptime", "wifi_signal", "volume", "cpu_temperature", "memory_available"} {
		t.Run(failing, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			psk := testPSK(t)
			s := testServer(t, psk)
			want := stubSensors(s)
			fail := func(f *func() (float32, bool)) { *f = func() (float32, bool) { return 0, false } }
			var failedKey uint32
			switch failing {
			case "uptime":
				fail(&s.uptime)
				failedKey = s.keyUptime
			case "wifi_signal":
				fail(&s.wifi)
				failedKey = s.keyWifi
			case "volume":
				fail(&s.volume)
				failedKey = s.keyVolume
			case "cpu_temperature":
				fail(&s.cpu)
				failedKey = s.keyCPU
			case "memory_available":
				fail(&s.memory)
				failedKey = s.keyMemory
			}

			pollAll(s)

			c, err := dial(t, s, psk)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.send(msgSubscribeStates, nil); err != nil {
				t.Fatal(err)
			}

			seen := false
			for n := 0; n < len(want); n++ {
				_, payload, err := c.recv()
				if err != nil {
					t.Fatalf("only %d of the %d readings arrived: %v", n, len(want), err)
				}
				key, value, missing := sensorReading(t, payload)
				if key != failedKey {
					if missing {
						t.Errorf("key %d was marked missing, and it was read successfully", key)
					}
					continue
				}
				seen = true
				if !missing {
					t.Errorf("a reading that failed went out as %v, which Home Assistant draws "+
						"as a measurement", value)
				}
			}
			if !seen {
				t.Errorf("the reading that failed was left out entirely; Home Assistant keeps " +
					"showing the last value it had")
			}
		})
	}
}

// The minute tick is what keeps the uptime and the signal fresh, and nothing
// else reads them. The three on the short tick must not ride along: they are
// published when they change, and a minute tick repeating them would undo
// that.
func TestTheMinuteTickCarriesOnlyItsOwnSensors(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	first := stubSensors(s)
	pollAll(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(first); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	// Every reading moves, so anything the tick carries arrives and anything it
	// does not carry is visibly absent.
	s.uptime = func() (float32, bool) { return 5678, true }
	s.wifi = func() (float32, bool) { return -70, true }
	s.volume = func() (float32, bool) { return 90, true }
	s.cpu = func() (float32, bool) { return 55.5, true }
	s.memory = func() (float32, bool) { return 64.5, true }
	s.publish("sensors", s.readTicked())

	// A ping behind the tick. One queue per connection and in order, so the
	// answer arriving marks the end of what the tick sent, and anything from
	// the short tick would have had to arrive first.
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}

	want := map[uint32]float32{s.keyUptime: 5678, s.keyWifi: -70}
	live := map[uint32]string{
		s.keyVolume: "volume", s.keyCPU: "cpu temperature", s.keyMemory: "memory",
	}
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("a polled reading did not arrive: %v", err)
		}
		if msgType == msgPingResponse {
			break
		}
		key, value, _ := sensorReading(t, payload)
		if name, isLive := live[key]; isLive {
			t.Fatalf("the minute tick repeated the %s, which the short tick already sends when it changes", name)
		}
		expected, known := want[key]
		if !known {
			t.Fatalf("the poll sent key %d, which is no ticked sensor of ours", key)
		}
		if value != expected {
			t.Errorf("the poll sent %v for key %d, want %v", value, key, expected)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("the poll never sent %d of the readings that have no other source", len(want))
	}
}

// aioesphomeapi pings only when it has not heard from the device, and cancels
// the pending one on any message. A device that talks often enough is never
// pinged, and a read deadline waiting for that ping expires on a connection
// that is working. So the deadline is satisfied by a ping of our own.
func TestAQuietConnectionIsPingedRatherThanDropped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = 150 * time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	// One message first: the longer deadline is bought by a frame that decrypts,
	// not by finishing the handshake, and until then the handshake budget runs.
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}

	// Then say nothing at all, twice over, answering each ping. A client that only
	// ever answers is one the deadline must not drop.
	for round := 1; round <= 2; round++ {
		msgType, _, err := c.recv()
		if err != nil {
			t.Fatalf("round %d: no ping arrived: %v", round, err)
		}
		if msgType != msgPingRequest {
			t.Fatalf("round %d: got message type %d, want a ping (%d)", round, msgType, msgPingRequest)
		}
		if err := c.send(msgPingResponse, nil); err != nil {
			t.Fatalf("round %d: answering the ping: %v", round, err)
		}
	}
}

// The ping is asked once. A peer that will not answer it is gone, and the
// budget it is gone after is ESPHome's, two and a half pings.
func TestAPeerThatWillNotAnswerThePingIsDropped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = 150 * time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingRequest {
		t.Fatalf("no ping arrived: type %d, %v", msgType, err)
	}

	// Answer nothing. The next deadline has no second ping to spend.
	if _, _, err := c.recv(); err == nil {
		t.Error("a peer that never answered the ping was still connected")
	}
}

// ESPHome's own shape: its device pings after KEEPALIVE_TIMEOUT_MS of silence
// and gives up at two and a half times it, so a client that is slow to answer
// is given longer than one that has merely gone quiet.
func TestTheKeepaliveBudgetMatchesESPHome(t *testing.T) {
	if pingAfter != 60*time.Second {
		t.Errorf("pingAfter is %v, want ESPHome's KEEPALIVE_TIMEOUT_MS of 60s", pingAfter)
	}
	// The literal, not pingAfter*5/2: that would restate the line that defines
	// idleWait and could not fail.
	if idleWait != 150*time.Second {
		t.Errorf("idleWait is %v, want ESPHome's KEEPALIVE_DISCONNECT_TIMEOUT of 150s", idleWait)
	}

	// What a Dot actually runs with. Every other test here shrinks pingWait, so
	// without this nothing reads what NewServer sets.
	if live := NewServer("dot", "model", "00:00:5E:00:53:00", make([]byte, noisePSKLen)); live.pingWait != 60*time.Second {
		t.Errorf("NewServer starts a connection on %v, want %v", live.pingWait, pingAfter)
	}

	// The two waits the read loop actually spends have to add up to that budget,
	// or the constant documents a timeout the code does not keep.
	s := &Server{pingWait: pingAfter}
	if spent := s.readWait(false) + s.readWait(true); spent != idleWait {
		t.Errorf("the read loop spends %v before it gives up, want %v", spent, idleWait)
	}
	if s.readWait(true) <= s.readWait(false) {
		t.Error("the wait after the ping is not the longer one, so a slow answer costs the connection")
	}
}

// The ping is for a peer that has proved it holds the key and then gone quiet.
// A peer that finishes the handshake and sends nothing has proved nothing --
// message 1 replays verbatim -- and gets the handshake budget and no more. Ping
// it and that budget doubles, which is a slot held twice as long for free.
func TestAPeerThatHasProvedNothingIsNotPinged(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.handshakeWait = 600 * time.Millisecond
	s.pingWait = 50 * time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	// Say nothing. The handshake budget runs out and that is the whole of it.
	start := time.Now()
	msgType, _, err := c.recv()
	took := time.Since(start)
	if err == nil {
		t.Errorf("a peer that sent nothing was answered with message type %d", msgType)
	}
	if msgType == msgPingRequest {
		t.Error("a peer that never decrypted a frame was pinged, which buys it a second deadline")
	}
	// Which end hung up matters as much as that one did: the client carries a
	// deadline of its own, and without this the test passes against a server
	// that holds the slot for ever.
	if took > 3*s.handshakeWait {
		t.Errorf("the connection lasted %v, well past the %v budget: the client's own deadline ended it, not the server",
			took, s.handshakeWait)
	}
}

// The asymmetry is spent on the client: a peer that has been asked something
// waits longer than one that has merely gone quiet. Measured as one span from a
// single mark rather than as two intervals either side of the ping, because
// those two are anti-correlated -- noticing the ping late inflates the first and
// shrinks the second by the same amount, so a scheduling hiccup of a few tens of
// milliseconds fails an honest server. This span only ever grows.
//
// A whole second rather than the milliseconds the other tests use, because the
// band has to separate 2.5 from the 3 a hard-coded readWait(true) spends, and
// what a loaded machine adds is a fixed number of milliseconds rather than a
// proportion. Measured under one emulated ARM cpu against eight busy ones, a
// truthful run reached 2.99 at 300ms, which is inside the mutant. Widening the
// band cannot fix that and scaling it can.
func TestTheWaitAfterThePingIsSpentOnTheClient(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = time.Second

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}

	start := time.Now()
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingRequest {
		t.Fatalf("no ping arrived: type %d, %v", msgType, err)
	}
	if _, _, err := c.recv(); err == nil {
		t.Fatal("a peer that never answered the ping was still connected")
	}
	lived := time.Since(start)

	// Two and a half pingWaits. Hard-coding either branch of readWait at the call
	// site gives two (both short) or three (both long), and the band is wide
	// enough that only those land outside it.
	if low, high := s.pingWait*11/5, s.pingWait*14/5; lived < low || lived > high {
		t.Errorf("a quiet connection lasted %v, want about %v (between %v and %v): the two waits are not %v then half again",
			lived, s.pingWait*5/2, low, high, s.pingWait)
	}
}

// The ping takes over a read that expired, so it may only do so when that read
// took nothing off the socket. io.ReadFull copies what it got into a buffer the
// caller drops with the error, so retrying a frame that stopped part-way reads
// the rest of it as a fresh header and every frame after that is garbage. Both
// of the reads it makes can stop that way, and the payload one always has the
// header behind it.
func TestAFrameThatStoppedPartWayIsNotResumed(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent int // bytes of the frame the server is given before the stall
	}{
		{"part of the header", 2},
		{"the whole header and none of the payload", 3},
		{"the header and part of the payload", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			psk := testPSK(t)
			s := testServer(t, psk)
			s.pingWait = 150 * time.Millisecond

			c, err := dial(t, s, psk)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.send(msgPingRequest, nil); err != nil {
				t.Fatal(err)
			}
			if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
				t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
			}

			// A whole valid frame, of which the server is given only the front.
			inner := make([]byte, 4)
			binary.BigEndian.PutUint16(inner[0:2], uint16(msgPingRequest))
			sealed, err := c.out.Encrypt(nil, nil, inner)
			if err != nil {
				t.Fatal(err)
			}
			frame := make([]byte, 3, 3+len(sealed))
			frame[0] = leadEncrypted
			binary.BigEndian.PutUint16(frame[1:3], uint16(len(sealed)))
			frame = append(frame, sealed...)
			if tc.sent >= len(frame) {
				t.Fatalf("the frame is only %d bytes, so %d of it is all of it", len(frame), tc.sent)
			}
			// Inside pingWait of the server's last read, or the stall under test is
			// the wait rather than the frame. One small encrypt stands between the
			// two, so the margin is wide.
			if _, err := c.conn.Write(frame[:tc.sent]); err != nil {
				t.Fatal(err)
			}

			// The deadline now expires part-way through. Answering it with a ping
			// would leave the server inside a frame it can never resynchronise.
			msgType, _, err := c.recv()
			if err == nil {
				t.Fatalf("a half-read frame was answered with message type %d rather than dropped", msgType)
			}
			if msgType == msgPingRequest {
				t.Error("the server pinged part-way through a frame, so the rest of it becomes a header")
			}
			// The operator's only signal. Blaming the peer for a lead byte this end
			// lost is the failure the mark exists to prevent being reported as.
			if said := out.String(); !strings.Contains(said, "mid-frame") {
				t.Errorf("the log does not say the stream was left mid-frame: %s", said)
			}
		})
	}
}

// Only a deadline is a peer that has gone quiet. Every other read error is a
// peer that said something wrong, and pinging it spends the one ping this
// connection has on a question already answered.
func TestOnlyAnExpiredReadDrawsAPing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	// Under the client's own deadline, so a drop this test sees is the server's,
	// and far enough above an immediate one that a ping could not be mistaken for
	// the error's doing.
	s := testServer(t, psk)
	s.pingWait = 2 * time.Second

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}

	// A frame that will not decrypt. The deadline is ten seconds away, so a ping
	// arriving here came from the error rather than from silence.
	if err := writeNoiseFrame(c.w, []byte("not sealed under the key at all")); err != nil {
		t.Fatal(err)
	}
	if err := c.w.Flush(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	msgType, _, err := c.recv()
	took := time.Since(start)
	if err == nil {
		t.Fatalf("a frame that failed to decrypt was answered with message type %d", msgType)
	}
	if msgType == msgPingRequest {
		t.Error("a peer that sent an undecryptable frame was pinged rather than dropped")
	}
	// The error ends it, so it ends at once. Left unbounded this passes on any
	// deadline that happens to expire later, the client's included.
	if took > s.pingWait/2 {
		t.Errorf("the drop took %v, long enough that a deadline ended it rather than the frame", took)
	}
}

// Read every tick, sent only when it moves. The uptime changes on every read
// and the signal does not, so what a quiet minute costs is the read alone.
func TestAReadingIsPublishedOnlyWhenItChanges(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	// Nothing is published yet, so the snapshot is empty and the first poll is
	// what fills it.
	pollAll(s)
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the first readings did not arrive: %v", err)
		}
	}

	// The others are held still, so every push below is the signal's.
	s.uptime = func() (float32, bool) { return 1234, true }
	s.volume = func() (float32, bool) { return 40, true }
	s.cpu = func() (float32, bool) { return 41.3, true }
	reads := func(v float32, ok bool) { s.wifi = func() (float32, bool) { return v, ok } }
	signal := func() []reading { return pollAll(s) }

	// stubSensors reads -48, so that is what the first poll above published.
	reads(-55, true)
	if got := signal(); len(got) != 1 {
		t.Fatal("a changed signal was not published")
	}
	if got := signal(); len(got) != 0 {
		t.Error("a reading equal to the published one was sent again")
	}

	reads(-60, true)
	signal()

	for _, expect := range []float32{-55, -60} {
		_, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the push carrying %v did not arrive: %v", expect, err)
		}
		key, value, missing := sensorReading(t, payload)
		if key != s.keyWifi || value != expect || missing {
			t.Errorf("a push carried key %d value %v missing %v, want the signal (%d) at %v",
				key, value, missing, s.keyWifi, expect)
		}
	}

	// A read that starts failing is a change even when the number it returns is
	// the one already published, or a signal that reached zero and then became
	// unreadable stays on screen as a real zero.
	reads(0, true)
	signal()
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("the zero did not arrive: %v", err)
	}
	reads(0, false)
	signal()
	_, payload, err := c.recv()
	if err != nil {
		t.Fatalf("the missing reading did not arrive: %v", err)
	}
	if _, _, missing := sensorReading(t, payload); !missing {
		t.Error("a reading that could not be taken went out as a measurement")
	}
}

// Nothing published is not the same as a published zero. A reading of zero that
// could not be taken at all is the zero value in both fields, and it is the
// first thing a Dot with no volume to read would have to say.
func TestAnUnreadableFirstReadingIsStillPublished(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	if got := s.publish("volume", []reading{{s.keyVolume, 0, false}}); len(got) != 1 {
		t.Error("the first reading was not published, so a volume that cannot be read is never reported")
	}
	if got := s.publish("volume", []reading{{s.keyVolume, 0, false}}); len(got) != 0 {
		t.Error("the same unreadable volume was published twice")
	}
}

// The defect that makes the published state a single thing: two readers of the
// device put a subscriber permanently out of step. The snapshot must replay
// what was published rather than take a reading of its own, or a value it alone
// saw is one the poll will never correct. Reproduced on the Dot with the volume
// before this was written, which is the reading a user turns and turns back.
func TestASubscriberIsNeverLeftHoldingAValueThePollWillNotCorrect(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	s.uptime = func() (float32, bool) { return 1234, true }

	// Published: 40.
	pollAll(s)

	// Turned to 50 between ticks, and a subscriber arrives inside that window.
	s.volume = func() (float32, bool) { return 50, true }
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	var told float32
	for n := 0; n < sensorCount; n++ {
		_, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
		if key, v, _ := sensorReading(t, payload); key == s.keyVolume {
			told = v
		}
	}

	// Turned back before the next tick, so the poll sees no change at all.
	s.volume = func() (float32, bool) { return 40, true }
	if got := pollAll(s); len(got) != 0 {
		t.Fatal("the poll published; this test no longer covers the case it was written for")
	}

	if told != 40 {
		t.Errorf("the subscriber was told %v and the device reads 40, with no push left to correct it", told)
	}
}

// A writer that records whether the server lock was held while a line was
// written to it.
type lockWatchingWriter struct {
	s      *Server
	mu     sync.Mutex
	held   bool
	writes int
}

func (w *lockWatchingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.s.mu.TryLock() {
		w.s.mu.Unlock()
	} else {
		w.held = true
	}
	return len(p), nil
}

// The lock gates the accept path and every other connection's handler, so a
// write to /data underneath it stalls the server. publish is where that rule
// now lives for every reading, which is the whole reason it is one function.
func TestPublishLogsWhatItCouldNotSendAfterDroppingTheLock(t *testing.T) {
	s := testServer(t, testPSK(t))
	w := &lockWatchingWriter{s: s}
	was := log.Writer()
	log.SetOutput(w)
	defer log.SetOutput(was)

	// A subscriber whose queue is already full, so the send fails and the
	// failure has to be reported.
	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	stalled := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame), states: true}
	s.mu.Lock()
	s.conns[stalled] = struct{}{}
	s.mu.Unlock()

	s.publish("volume", []reading{{s.keyVolume, 40, true}})

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writes == 0 {
		t.Fatal("nothing was logged, so this test would pass on a publish that never reports a failure")
	}
	if w.held {
		t.Error("a failure was logged with the server lock held")
	}
}

// eachConn drops a connection whose send failed, and it can only know to do
// that if sendSensorsAt stops at the first failure and says so. Carrying on
// would leave a subscriber holding some of a push and still in the table.
func TestAFailedSendDropsTheConnectionRatherThanContinuing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	// Room for one frame, and two readings to send.
	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	stalled := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame, 1), states: true}
	s.mu.Lock()
	s.conns[stalled] = struct{}{}
	s.mu.Unlock()

	s.publish("sensors", []reading{
		{s.keyUptime, 1, true},
		{s.keyWifi, -48, true},
		{s.keyVolume, 40, true},
	})

	s.mu.Lock()
	_, still := s.conns[stalled]
	s.mu.Unlock()
	if still {
		t.Error("a connection that could not take a whole push is still in the table")
	}
	// The scenario rather than the behaviour: a queue that refused the first
	// frame too would make the drop above prove nothing about stopping. What
	// carries this test is the connection being gone.
	if n := len(stalled.out); n != 1 {
		t.Errorf("the stalled connection was queued %d frames, so this is not the case the test means to cover", n)
	}
}

// The read forks a process, so it is not made while nothing is listening -- and
// a connection that has not subscribed is not listening either. What makes that
// safe is that subscribing wakes the poll rather than reading for itself, so
// the tick here is long enough that only the wake can deliver in time.
func TestTheLivePollSleepsUntilSomebodySubscribes(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)

	var mu sync.Mutex
	reads := 0
	s.volume = func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	}

	// Connected before the poll starts, and saying nothing: the poll's first
	// look has a connection to see, and a poll that reads for one that has not
	// subscribed is reading for nobody.
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	go s.PollLive(30 * time.Second)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	idle := reads
	mu.Unlock()
	if idle != 0 {
		t.Errorf("the poll read the device %d times for a connection that never subscribed", idle)
	}

	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	// Nothing has published at all, so this can only arrive because subscribing
	// woke the poll: the next tick is thirty seconds away.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting for the volume: %v", err)
		}
		if key, v, _ := sensorReading(t, payload); key == s.keyVolume {
			if v != 40 {
				t.Errorf("the woken poll published %v, want 40", v)
			}
			return
		}
	}
	t.Error("subscribing did not wake the volume poll, so the reading waits for a tick that is half a minute away")
}

// The wake is what lets the poll sleep, so asking for it has to cost nothing
// after the first time. The volume read forks a process, and a peer holding the
// key can send SubscribeStatesRequest as fast as it likes: repeating the wake
// would hand it a fork per request, on a device with 512 MiB of memory.
func TestResubscribingDoesNotBuyAnotherReading(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	var mu sync.Mutex
	reads := 0
	count := func(v float32) func() (float32, bool) {
		return func() (float32, bool) { mu.Lock(); reads++; mu.Unlock(); return v, true }
	}
	s.volume, s.uptime, s.wifi = count(40), count(1234), count(-48)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	// Ticks far enough away that only a wake can cause a read.
	go s.PollLive(time.Hour)
	go s.PollSensors(time.Hour)

	ask := func(i int) {
		t.Helper()
		if err := c.send(msgSubscribeStates, nil); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		for n := 0; n < sensorCount; n++ {
			if _, _, err := c.recv(); err != nil {
				t.Fatalf("draining subscribe %d: %v", i, err)
			}
		}
	}

	// The first one is allowed to wake both polls; what it costs is not the
	// point here, so it is measured rather than assumed.
	ask(0)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	first := reads
	mu.Unlock()
	if first == 0 {
		t.Fatal("the first subscribe woke nothing, so this test would pass on a server that never reads")
	}

	const asks = 50
	for i := 1; i <= asks; i++ {
		ask(i)
	}
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if reads != first {
		t.Errorf("%d further subscribe requests drew %d more readings of the device; a peer holding the key can ask as fast as it likes",
			asks, reads-first)
	}
}

// The snapshot replays what was published, so a subscriber arriving between
// ticks is answered with a value up to a whole tick old. Waking the sensor poll
// as well as the volume one is what keeps that to a read rather than to a
// minute.
func TestSubscribingWakesTheSensorPoll(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	// The reading moves, not the function: PollSensors calls this from its own
	// goroutine, so swapping s.uptime underneath it is a race on the field.
	var mu sync.Mutex
	uptime := want[s.keyUptime]
	read := make(chan struct{}, 1)
	s.uptime = func() (float32, bool) {
		mu.Lock()
		defer mu.Unlock()
		select {
		case read <- struct{}{}:
		default:
		}
		return uptime, true
	}
	pollAll(s)
	<-read

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	// Started, and parked on a tick an hour away, while the reading is still the
	// published one: its own first publish must not be what delivers the new
	// value, or this passes without any wake at all. Waited for rather than
	// slept past, because a sleep that a loaded machine outruns turns this into
	// a test that passes with the wake deleted.
	go s.PollSensors(time.Hour)
	<-read

	// Only now does the device move on.
	mu.Lock()
	uptime = 9999
	mu.Unlock()

	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	// The deadline is the client's, so a wake that never comes ends this rather
	// than the loop spinning until dial's own five seconds run out.
	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		_, payload, err := c.recv()
		if err != nil {
			t.Fatalf("subscribing left the uptime at its published value with no read to correct it before the next tick: %v", err)
		}
		if key, v, _ := sensorReading(t, payload); key == s.keyUptime && v == 9999 {
			return
		}
	}
}

// The wake is what starts the poll; the tick is what keeps it going. A poll
// that only ever woke would read once for each subscriber and never again,
// which is the headline behaviour of this sensor gone.
func TestTheLivePollKeepsReadingOnItsOwnTick(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	var mu sync.Mutex
	reads := 0
	s.volume = func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	pollAll(s)
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	mu.Lock()
	before := reads
	mu.Unlock()

	// Subscribed once, then left alone. Only the ticker can read now.
	go s.PollLive(20 * time.Millisecond)
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if reads-before < 3 {
		t.Errorf("the poll read %d times in 400ms on a 20ms tick; a poll that only wakes reads once and stops",
			reads-before)
	}
}

// A subscriber is answered from the published state, so something has to have
// published before the first tick comes round: a Dot whose sensor tick is a
// minute would otherwise answer its first subscriber with nothing at all.
func TestTheSensorPollPublishesBeforeItsFirstTick(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)

	go s.PollSensors(time.Hour)
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.published) == 0 {
		t.Error("nothing was published before the first tick, so a subscriber arriving inside it is told nothing")
	}
}

// Only what changed goes on the wire, not the whole batch it arrived in. The
// volume publishes one reading at a time, so this needs the two the minute tick
// carries together.
func TestOnlyTheChangedReadingOfABatchIsSent(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < sensorCount; n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	// One of the two moves; the other is exactly what was published.
	s.publish("sensors", []reading{
		{s.keyUptime, 4321, true},
		{s.keyWifi, stubSensors(s)[s.keyWifi], true},
	})
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}

	// A ping behind the push: one queue per connection and in order, so the
	// answer marks the end of what the push sent.
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting on the push: %v", err)
		}
		if msgType == msgPingResponse {
			return
		}
		key, value, _ := sensorReading(t, payload)
		if key != s.keyUptime {
			t.Errorf("a reading equal to the published one was sent again: key %d carried %v", key, value)
		}
	}
}

// A subscriber is answered from the published state, so it needs a reading of
// its own only when there was nobody to keep that state current. With one
// already subscribed the polls have been running, and another connection is
// answered for free however many of them arrive.
func TestASecondSubscriberCostsNoReading(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	var mu sync.Mutex
	reads := 0
	s.volume = func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return reads }

	// A tick far away, so a read here is one a connection asked for; and a gap
	// short enough that it is the idle check, not the gap, deciding.
	s.liveGap = 10 * time.Millisecond
	go s.PollLive(time.Hour)
	time.Sleep(100 * time.Millisecond)

	subscribe := func(what string) *client {
		t.Helper()
		c, err := dial(t, s, psk)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := c.send(msgSubscribeStates, nil); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for n := 0; n < sensorCount; n++ {
			if _, _, err := c.recv(); err != nil {
				t.Fatalf("%s snapshot: %v", what, err)
			}
		}
		time.Sleep(120 * time.Millisecond)
		return c
	}

	before := count()
	subscribe("the first subscriber")
	first := count() - before
	if first == 0 {
		t.Fatal("the first subscriber woke nothing, so the published volume it was answered from could be any age")
	}

	after := count()
	for i := 0; i < 3; i++ {
		subscribe("a later subscriber")
	}
	if got := count() - after; got != 0 {
		t.Errorf("three connections arriving after one was already subscribed drew %d readings; the published state was already current",
			got)
	}
}

// Waking is for the idle case, so what is left of it is a peer that keeps
// making itself the idle case: connect, subscribe, leave, repeat. Each cycle
// would otherwise be a fork, at whatever rate handshakes can be done. The gap
// is what bounds that, and it bounds the wake alone -- the tick is a cadence
// somebody chose.
func TestWakingAgainInsideTheGapReadsNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	var mu sync.Mutex
	reads := 0
	s.volume = func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return reads }

	s.liveGap = 2 * time.Second
	go s.PollLive(time.Hour)
	time.Sleep(100 * time.Millisecond)

	// Subscribe, then go away again, twice over, well inside the gap.
	churn := func(round int) {
		c, err := dial(t, s, psk)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if err := c.send(msgSubscribeStates, nil); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		for n := 0; n < sensorCount; n++ {
			if _, _, err := c.recv(); err != nil {
				t.Fatalf("round %d snapshot: %v", round, err)
			}
		}
		time.Sleep(80 * time.Millisecond)
		c.conn.Close()
		// Wait for the server to notice, so the next round is the idle case
		// again rather than a second subscriber.
		for i := 0; i < 100; i++ {
			if !s.anyStateSubscriber() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("round %d: the connection was never dropped", round)
	}

	before := count()
	churn(1)
	first := count() - before
	if first == 0 {
		t.Fatal("the first subscriber read nothing, so this test would pass on a server that never reads")
	}
	churn(2)
	churn(3)

	if got := count() - before; got != first {
		t.Errorf("three idle-to-active cycles inside a %v gap drew %d readings, want the %d of the first",
			s.liveGap, got, first)
	}
}

// Every sensor that is listed is a sensor something has to read. A listing and
// a set of polls that disagree leave an entity that exists and never gets a
// state: unavailable in Home Assistant, and green in every other test here.
func TestEveryListedSensorHasAPollThatPublishesIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)

	want := map[uint32]bool{}
	for _, entity := range listed(t, s) {
		want[uint32(entity[2].num)] = true
	}
	if len(want) == 0 {
		t.Fatal("nothing is listed, so this test would pass on a server that polls nothing")
	}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	s.Poll(MinSensorTick, 20*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for len(want) > 0 && time.Now().Before(deadline) {
		_, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting on the polls: %v", err)
		}
		key, _, _ := sensorReading(t, payload)
		delete(want, key)
	}
	for key := range want {
		t.Errorf("sensor %d is listed and no poll ever published it", key)
	}
}

// time.NewTicker panics on a tick that is not positive, and PollLive has no
// MinSensorTick to have stopped one. A panic here takes the daemon down, and
// the supervisor brings it back to do the same again five seconds later.
func TestTheLivePollSurvivesATickThatIsNotPositive(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)

	go s.PollLive(0)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(out.String(), "raised to") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a tick of zero was accepted; the log says %q", out.String())
}

// The told flag, which every real key hides: entityKey is a hash and none of
// them is zero, so a reading that is the zero value in every field is the only
// thing that can tell "published" from "never published" apart. Without the
// flag it reads as already published and is never sent at all.
func TestAReadingThatIsZeroInEveryFieldIsStillPublished(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	if got := s.publish("sensors", []reading{{}}); len(got) != 1 {
		t.Error("a reading that is zero in every field was taken for one already published")
	}
	if got := s.publish("sensors", []reading{{}}); len(got) != 0 {
		t.Error("the same reading was published twice")
	}
}

// A wake is sent with the server lock held, so it must never block. A bare send
// would deadlock the whole server: handle would hold the lock against every
// other connection and the accept path, while the poll that drains the channel
// is itself waiting on that lock inside publish. Nothing else here can see
// that, because every other test sends one wake into an empty buffer.
func TestAWakeThatCannotBeSentIsDroppedRatherThanWaitedOn(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)

	// Both filled, with no poll running to drain either.
	s.sensorWake <- struct{}{}
	s.liveWake <- struct{}{}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	// A ping behind the subscribe: one queue per connection and in order, so an
	// answer to it is the handler having come back from the wake.
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		msgType, _, err := c.recv()
		if err != nil {
			t.Fatalf("the handler never came back from a wake it could not send, and it holds the server lock: %v", err)
		}
		if msgType == msgPingResponse {
			return
		}
	}
}

// The drain counts in this file are written against sensorCount, and a sensor
// added without moving it would leave every one of them one frame short --
// which surfaces as an unrelated test hanging on a read, rather than as this.
func TestTheSensorCountMatchesTheListing(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	if got := len(listed(t, s)); got != sensorCount {
		t.Errorf("the server lists %d sensors and sensorCount is %d", got, sensorCount)
	}
}
