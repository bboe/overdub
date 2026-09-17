package sendspin

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/untrustedlog"
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestServeRefusesPastTheConnectionCap(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	var held []net.Conn
	for range maxConns {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { nc.Close() })
		held = append(held, nc)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := c.conns()
		if n >= maxConns {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d connections were accepted", n, maxConns)
		}
		time.Sleep(5 * time.Millisecond)
	}

	extra, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer extra.Close()
	if err := extra.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err = extra.Read(make([]byte, 1))
	switch {
	case err == nil:
		t.Error("the connection past the cap was not closed")
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Error("the connection past the cap was left open: nothing bounds the accept loop")
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
	default:
		t.Errorf("the connection past the cap ended with an unexpected error: %v", err)
	}
	_ = held
}

func TestPeerLinesSayWhichSurfaceSpentTheBudget(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	client := &Client{Config: Config{Name: "kitchen"}}
	serveOn(t, client, ln)

	deadline := time.Now().Add(5 * time.Second)
	for client.subject() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Serve never named the subject its peer lines are spent under")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := client.subject(); got != "sendspin" {
		t.Errorf("peer lines are spent under %q, want %q: an unattributed suppressed-count"+
			" line does not say which surface a peer was spending against", got, "sendspin")
	}
	ln.Close()
}

func spendTheBudget(c *Client) {
	for i := 0; i < untrustedlog.Burst; i++ {
		c.Peer.Printf("sendspin: line %d", i)
	}
}

func TestAnActivationSpendsThePeerBudget(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	spendTheBudget(c)
	before := len(out.String())

	for i := 0; i < 20; i++ {
		peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
			Activities:  []string{activityPlayback},
			ActiveRoles: roles(rolePlayerV1),
		}))
	}
	time.Sleep(200 * time.Millisecond)

	if wrote := len(out.String()) - before; wrote > 0 {
		t.Errorf("twenty activations wrote %d bytes past the budget; a peer can repeat"+
			" server/activate for as long as it likes:\n%s", wrote, out.String()[before:])
	}
}

func TestManyRolesCannotStretchOneLogLine(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	repeated := make([]string, 3000)
	for i := range repeated {
		repeated[i] = rolePlayerV1
	}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: &repeated,
	}))
	time.Sleep(200 * time.Millisecond)

	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 512 {
			t.Errorf("a peer stretched one log line to %d bytes by repeating a role", len(line))
		}
	}
}

func TestGivingUpEveryRoleGivesUpTheSlot(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{},
		ActiveRoles: &none,
	}))

	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		held := c.held
		c.mu.Unlock()
		if held == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a session holding no role still holds the slot, so no other server can" +
				" ever be admitted while it stays connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnAbsentRoleListLeavesTheSlotAlone(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities: []string{activityPlayback},
	}))
	time.Sleep(200 * time.Millisecond)

	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if held == nil {
		t.Error("an activation that named no roles at all gave up the slot; silence narrows a" +
			" connection rather than relinquishing what it already holds")
	}
}

func TestAPeerStringInAnErrorCannotForgeLogLines(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	peer := dialLocal(t, ln)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)
	forged := "boom\n2026/09/12 13:00:00 sendspin: handshake with \"evil\" complete\n"
	reason, err := json.Marshal(serverErrorPayload{Reason: forged + strings.Repeat("z", 1500)})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(envelope{Type: typeServerError, Payload: reason})
	if err != nil {
		t.Fatal(err)
	}
	peer.writeText(frame)
	time.Sleep(200 * time.Millisecond)

	got := out.String()
	if strings.Contains(got, "handshake with \"evil\" complete") {
		t.Errorf("a peer forged a log line of its own:\n%s", got)
	}
	if lines := strings.Count(strings.TrimRight(got, "\n"), "\n"); lines > 2 {
		t.Errorf("one peer string wrote %d physical log lines:\n%s", lines+1, got)
	}
}

func TestAGroupNameCannotStretchOrForgeALogLine(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{
		GroupName:     strings.Repeat("a", 3000),
		PlaybackState: "playing\nsendspin: forged",
	}))
	time.Sleep(200 * time.Millisecond)

	saw := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "sendspin: group") {
			saw = true
		}
		if len(line) > 512 {
			t.Errorf("a peer stretched one log line to %d bytes with a group name", len(line))
		}
		if strings.HasPrefix(line, "sendspin: forged") {
			t.Error("a newline in a group's playback state forged a log line of its own")
		}
	}
	if !saw {
		t.Fatal("the group line was never written, so nothing here was tested")
	}
}

func TestASessionHoldingNoRoleDoesNotKeepItsConnectionSlot(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.rolelessAfter = 200 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{},
		ActiveRoles: &none,
	}))

	time.Sleep(400 * time.Millisecond)
	peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{GroupName: "any"}))

	deadline := time.Now().Add(5 * time.Second)
	for {
		conns := c.conns()
		if conns == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a session that gave up every role kept its connection slot; eight such" +
				" peers fill maxConns and Serve refuses music assistant before it handshakes")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAPeerThatNeverHandshakesLosesItsSlot(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.handshakeAfter = 200 * time.Millisecond
	serveOn(t, c, ln)

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { nc.Close() })
	waitForConns(t, c, 1, "the connection was never accepted")
	waitForConns(t, c, 0, "a peer that opened a socket and sent nothing held its slot")
}

func TestAPeerThatNeverActivatesLosesItsSlot(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.provisionalAfter = 200 * time.Millisecond
	serveOn(t, c, ln)

	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "quiet server"}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	waitForConns(t, c, 0, "a peer that handshook and never activated held its slot")
}

func waitForConns(t *testing.T, c *Client, want int, complaint string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := c.conns()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: conns = %d, want %d", complaint, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestARolelessSessionCannotHoldOnWithControlFrames(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.rolelessAfter = 200 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{},
		ActiveRoles: &none,
	}))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := peer.conn.Write(frame(true, opPing, []byte("keepalive"))); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	waitForConns(t, c, 0, "a roleless session held its slot for as long as it sent control frames")
}

func TestRegainingARoleCallsOffTheBound(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.rolelessAfter = 300 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{},
		ActiveRoles: &none,
	}))
	time.Sleep(100 * time.Millisecond)
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))

	time.Sleep(600 * time.Millisecond)
	conns := c.conns()
	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if conns != 1 || held == nil {
		t.Errorf("conns = %d, held = %v; declaring a role again must call off the bound",
			conns, held != nil)
	}
}

func TestRepeatedDeactivationDoesNotRestartTheBound(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.rolelessAfter = 300 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := peer.pushBinary(server.sealJSON(t, typeServerActivate, serverActivate{
				Activities:  []string{},
				ActiveRoles: &none,
			})); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	waitForConns(t, c, 0, "a peer held its slot by giving up its roles over and over")
}

func TestALongServerNameCannotStretchOrForgeALogLine(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{
		Name: "ma\nsendspin: forged" + strings.Repeat("n", 3000),
	}))
	time.Sleep(300 * time.Millisecond)

	saw := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "handshake with") {
			saw = true
		}
		if len(line) > 300 {
			t.Errorf("a peer's own name stretched a log line to %d bytes", len(line))
		}
		if strings.HasPrefix(line, "sendspin: forged") {
			t.Error("a newline in the server's name forged a log line")
		}
	}
	if !saw {
		t.Fatal("the handshake line was never written, so nothing here was tested")
	}
}

func TestTheKeepalivePingsAnIdleHolder(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.pingEvery = 100 * time.Millisecond
	serveOn(t, c, ln)
	peer, _, _ := bringUp(t, c, ln)

	if err := peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	op, _ := peer.read()
	if op != opPing {
		t.Errorf("the Dot sent %#x, want a ping (%#x)", op, opPing)
	}
}

func chunkFrame(t *testing.T, samples int) []byte {
	t.Helper()
	b := make([]byte, 1+8+samples)
	b[0] = 4
	binary.BigEndian.PutUint64(b[1:9], uint64(nowMicros()))
	return b
}

func TestAnAudioChunkDoesNotDropTheSession(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for range 3 {
		peer.writeBinary(server.seal(t, chunkFrame(t, 64)))
	}
	peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{GroupName: "after"}))
	time.Sleep(300 * time.Millisecond)

	conns := c.conns()
	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if conns != 1 || held == nil {
		t.Errorf("conns = %d, held = %v; an unhandled binary message ended the session",
			conns, held != nil)
	}
}

func TestOrdinaryTrafficDoesNotSpendThePeerBudget(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	before := c.Peer.Written()
	for range 200 {
		peer.writeBinary(server.sealJSON(t, typeStreamEnd, struct{}{}))
		peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
		peer.writeBinary(server.seal(t, []byte{0x40, 1, 2}))
		peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{
			PlaybackState: "playing", GroupID: "g1", GroupName: "kitchen",
		}))
		peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
			Activities:  []string{activityPlayback},
			ActiveRoles: roles(rolePlayerV1),
		}))
	}
	time.Sleep(500 * time.Millisecond)

	if spent := c.Peer.Written() - before; spent > 6 {
		t.Errorf("1000 ordinary messages spent %d lines of the peer budget; one per"+
			" distinct thing said is the most that says anything new", spent)
	}
	if !strings.Contains(out.String(), "not handled yet") {
		t.Error("the first unhandled message was never reported, so nothing was tested")
	}
}

func TestCyclingRolesCannotBuyMoreRolelessTime(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.rolelessAfter = 300 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := peer.pushBinary(server.sealJSON(t, typeServerActivate, serverActivate{
				Activities: []string{}, ActiveRoles: &none})); err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
			if err := peer.pushBinary(server.sealJSON(t, typeServerActivate, serverActivate{
				Activities: []string{activityPlayback}, ActiveRoles: roles(rolePlayerV1)})); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	waitForConns(t, c, 0, "a peer held its slot by cycling its roles rather than keeping one")
}

func TestTimeHoldingARoleIsNotChargedAsRoleless(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.rolelessAfter = 400 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	none := []string{}
	narrow := func() {
		peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
			Activities: []string{}, ActiveRoles: &none}))
	}
	widen := func() {
		peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
			Activities: []string{activityPlayback}, ActiveRoles: roles(rolePlayerV1)}))
	}

	narrow()
	time.Sleep(100 * time.Millisecond) // 100ms of the 400ms allowance
	widen()
	time.Sleep(600 * time.Millisecond) // working, and longer than the allowance
	narrow()
	time.Sleep(100 * time.Millisecond) // 200ms spent in total, still inside it
	widen()
	time.Sleep(100 * time.Millisecond)

	conns := c.conns()
	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if conns != 1 || held == nil {
		t.Errorf("conns = %d, held = %v; the time it spent holding a role was charged"+
			" against its roleless allowance", conns, held != nil)
	}
}

func TestALeavingSessionDoesNotReleaseAnotherServersHold(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	holder, _, _ := bringUp(t, c, ln)
	_ = holder
	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if held == nil {
		t.Fatal("the first server never took the hold")
	}

	other := dialLocal(t, ln)
	server := driveServer(t, other, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	other.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "second"}))
	if kind, _ := nextJSON(t, other, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	other.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	if kind, _ := nextJSON(t, other, server); kind != typeClientGoodbye {
		t.Fatalf("second server got %s, want %s", kind, typeClientGoodbye)
	}
	other.conn.Close()
	time.Sleep(300 * time.Millisecond)

	c.mu.Lock()
	still := c.held
	c.mu.Unlock()
	if still != held {
		t.Error("a refused server's disconnect released the hold the first server still has")
	}
}

func bringUpWith(t *testing.T, c *Client, ln net.Listener, plan serverPlan) (*wsPeer, *serverSide) {
	t.Helper()
	peer := dialLocal(t, ln)
	plan.clientPublic = c.Keys.Identity.Public
	server := driveServer(t, peer, plan)
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "paired server"}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	return peer, server
}

func TestTheConfiguredPairingKeyIsWhatTheHandshakeUses(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	bringUpWith(t, c, ln, serverPlan{psk: c.Keys.PairingPSK, cat: categoryPairing})
}

func TestCloseEndsLiveSessionsAndRefusesNewOnes(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	bringUp(t, c, ln)
	if c.conns() != 1 {
		t.Fatalf("conns = %d, want 1 before closing", c.conns())
	}

	c.Close()
	waitForConns(t, c, 0, "Close left a session running")

	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if held != nil {
		t.Error("Close left the exclusive slot held")
	}

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()
	if err := nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Error("a connection was accepted after Close")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("a connection after Close was left open rather than refused")
	}
}

func TestCloseSaysGoodbyeBeforeItDropsTheSession(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	c.Close()

	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientGoodbye {
		t.Fatalf("Close sent %s first, want %s", kind, typeClientGoodbye)
	}
	var said goodbye
	if err := json.Unmarshal(payload, &said); err != nil {
		t.Fatalf("decoding client/goodbye: %v", err)
	}
	if said.Reason != goodbyeShutdown {
		t.Errorf("reason = %q, want %q", said.Reason, goodbyeShutdown)
	}
	waitForConns(t, c, 0, "Close left a session running after the goodbye")
}

func TestAnEnvelopeTypeCannotForgeALogLine(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	long := strings.Repeat("a", 3000)
	peer.writeBinary(server.sealJSON(t,
		long+"\nsendspin: handshake with \"forged\" complete on the sn psk", struct{}{}))
	time.Sleep(200 * time.Millisecond)

	allowed := strings.TrimSuffix(untrustedlog.Cut(long), "...")
	if strings.Contains(out.String(), allowed+"a") {
		t.Error("the envelope type reached the log uncut, so the peer string is bounded" +
			" only by the per-line truncation")
	}

	saw := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "not handled yet") || strings.Contains(line, "ignoring") {
			saw = true
		}
		if len(line) > 512 {
			t.Errorf("a peer stretched one log line to %d bytes with an envelope type",
				len(line))
		}
		if strings.HasPrefix(line, "sendspin: handshake with") {
			t.Error("a newline in an envelope type forged a handshake line of its own")
		}
	}
	if !saw {
		t.Fatal("the unhandled-kind line was never written, so nothing here was tested")
	}
}

func TestTheReportedSetSaysEachThingOnceAndStopsGrowing(t *testing.T) {
	var noted noteSet
	if !noted.first("first mention") {
		t.Error("the first mention of something was suppressed")
	}
	if noted.first("first mention") {
		t.Error("a repeat was reported a second time")
	}
	for i := range maxNoted * 8 {
		noted.first(fmt.Sprintf("overdub/unknown-%d", i))
	}
	if got := len(noted.seen); got > maxNoted {
		t.Errorf("the set grew to %d kinds against a bound of %d, and it holds one cut"+
			" string each for the life of the connection", got, maxNoted)
	}
}

func TestRepeatingAnActivationCostsNeitherAStateNorAKeepalive(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for range 5 {
		peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
			Activities:  []string{activityPlayback},
			ActiveRoles: roles(rolePlayerV1),
		}))
	}

	if !peer.quiet(400 * time.Millisecond) {
		t.Error("a repeated activation was answered again: each one writes another" +
			" client/state and leaves another keepalive ticker running")
	}
}

func TestEveryStreamIsLoggedRatherThanOnlyTheFirst(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for range 3 {
		peer.writeBinary(server.sealJSON(t, typeStreamStart, streamStart{
			ServerTransmitted: 1, Player: &streamPlayer{
				Codec: codecPCM, SampleRate: StreamRate,
				Channels: StreamChannels, BitDepth: StreamBitDepth,
			}}))
		peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 2}))
	}
	time.Sleep(500 * time.Millisecond)

	if got := strings.Count(out.String(), "started a"); got != 3 {
		t.Errorf("three streams were logged %d times; a track after the first says"+
			" nothing, which is what made the second one unreadable on hardware", got)
	}
	if got := strings.Count(out.String(), "ended its stream"); got != 3 {
		t.Errorf("three stream ends were logged %d times", got)
	}
}

func TestAConnectionThatDropsMidStreamSaysWhatItHeard(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	peer.writeBinary(server.sealJSON(t, typeStreamStart, streamStart{
		ServerTransmitted: 1, Player: &streamPlayer{
			Codec: codecPCM, SampleRate: StreamRate,
			Channels: StreamChannels, BitDepth: StreamBitDepth,
		}}))
	for range 3 {
		peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	}
	time.Sleep(300 * time.Millisecond)
	peer.conn.Close()
	time.Sleep(500 * time.Millisecond)

	if !strings.Contains(out.String(), "3 chunks") {
		t.Error("a connection that dropped mid-stream threw away what it had counted," +
			" which is exactly the run somebody reads the log to ask about")
	}
}

func TestAChunkThisPlayerCannotReadDoesNotDropTheSession(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)

	bad := make([]byte, 1+chunkStampBytes+2)
	bad[0] = binaryAudioChunk
	binary.BigEndian.PutUint64(bad[1:9], uint64(stampCeiling+1))
	peer.writeBinary(server.seal(t, bad))
	time.Sleep(200 * time.Millisecond)

	peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 3}))
	time.Sleep(400 * time.Millisecond)

	if !strings.Contains(out.String(), "cannot read") {
		t.Error("a chunk this player could not read was dropped with nothing said")
	}
	if !strings.Contains(out.String(), "ended its stream") {
		t.Error("a chunk this player could not read closed the session, so the message" +
			" after it was never read; a server stamping every chunk the same way is" +
			" then never played at all")
	}
}

func playing(t *testing.T, peer *wsPeer, server *serverSide) {
	t.Helper()
	peer.writeBinary(server.sealJSON(t, typeStreamStart, streamStart{
		ServerTransmitted: 1, Player: &streamPlayer{
			Codec: codecPCM, SampleRate: StreamRate,
			Channels: StreamChannels, BitDepth: StreamBitDepth,
		}}))
}

func TestClearingMidStreamDoesNotReadAsAnotherStreamStarting(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	peer.writeBinary(server.sealJSON(t, typeStreamClear, streamRoles{ServerTransmitted: 2}))
	peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	time.Sleep(400 * time.Millisecond)

	if got := strings.Count(out.String(), "its first chunk"); got != 1 {
		t.Errorf("one stream announced a first chunk %d times; a clear leaves the stream"+
			" open, so the audio after it is the same stream carrying on", got)
	}
}

func TestLosingThePlayerRoleSummarisesTheStreamItAbandoned(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	time.Sleep(200 * time.Millisecond)

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities: []string{}, ActiveRoles: &none}))
	time.Sleep(300 * time.Millisecond)

	said := out.String()
	at := strings.Index(said, "1 chunks")
	if at < 0 {
		t.Fatal("the stream this client was told to abandon was never summarised, and its" +
			" counts are reported against whatever stream comes next")
	}
	if holds := strings.Index(said, "holds no role now"); holds >= 0 && at > holds {
		t.Error("the summary came after the role was given up, so it reads as belonging" +
			" to a stream that had not started")
	}
}

func badChunk(t *testing.T, stamp int64) []byte {
	t.Helper()
	b := make([]byte, 1+chunkStampBytes+2)
	b[0] = binaryAudioChunk
	binary.BigEndian.PutUint64(b[1:9], uint64(stamp))
	return b
}

func TestAServerStampingEveryChunkWrongDoesNotBlindTheLog(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	for i := range 40 {
		peer.writeBinary(server.seal(t, badChunk(t, stampCeiling+1+int64(i))))
	}
	time.Sleep(400 * time.Millisecond)

	peer.writeBinary(server.seal(t, []byte{0x40, 1, 2}))
	time.Sleep(400 * time.Millisecond)

	if got := strings.Count(out.String(), "cannot read"); got != 1 {
		t.Errorf("forty unreadable chunks were reported %d times; each carried the"+
			" server's own number, so each is a note of its own", got)
	}
	if !strings.Contains(out.String(), "not handled yet") {
		t.Error("the note budget was spent on one server's bad stamps, so nothing else" +
			" this connection does is ever logged again")
	}
}

func TestAStreamForAnotherRoleDoesNotSplitThePlayersSummary(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	peer.writeBinary(server.sealJSON(t, typeStreamStart, streamStart{ServerTransmitted: 2}))
	peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	time.Sleep(400 * time.Millisecond)

	if got := strings.Count(out.String(), "its first chunk"); got != 1 {
		t.Errorf("the player's stream announced a first chunk %d times; a stream/start"+
			" for artwork or a visualizer is not this player's stream ending", got)
	}
	if strings.Contains(out.String(), "1 chunks") {
		t.Error("a stream/start for another role flushed a summary for audio that was" +
			" still arriving on the player's own stream")
	}
}

func TestAStreamThatStoppedSendingAudioIsStillSummarised(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	c.reportEvery = 150 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	peer.writeBinary(server.seal(t, chunkFrame(t, 8)))
	time.Sleep(300 * time.Millisecond)

	if strings.Contains(out.String(), "1 chunks") {
		t.Fatal("a summary arrived with no message to carry it, so nothing here is tested")
	}
	peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{
		PlaybackState: "playing", GroupID: "g1", GroupName: "kitchen"}))
	time.Sleep(300 * time.Millisecond)

	if !strings.Contains(out.String(), "1 chunks") {
		t.Error("a stream that stopped sending audio said nothing more, which is the" +
			" starvation the lead number exists to make visible")
	}
}

func TestPlaybackCannotSpendTheBudgetTheHandshakeNeeds(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	before := c.Peer.Written()
	for range 300 {
		playing(t, peer, server)
		peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 2}))
	}
	time.Sleep(700 * time.Millisecond)

	if spent := c.Peer.Written() - before; spent > 2 {
		t.Errorf("a server flapping its stream spent %d lines of the budget the roles,"+
			" the clock and the handshake share, and that budget never refills", spent)
	}
	if c.Play.Written() == 0 {
		t.Error("the streams were not logged at all")
	}
}
