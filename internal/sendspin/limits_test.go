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
	"path/filepath"
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
	waitFor(t, "every connection up to the cap to be accepted",
		func() bool { return c.conns() >= maxConns })

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

	waitFor(t, "Serve to name the subject its peer lines are spent under",
		func() bool { return client.subject() != "" })
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
	handled(t, peer, server)

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
	handled(t, peer, server)

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

	waitFor(t, "a session holding no role to give up the slot, or no other server can"+
		" ever be admitted while it stays connected", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.held == nil
	})
}

func TestAnAbsentRoleListLeavesTheSlotAlone(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities: []string{activityPlayback},
	}))
	handled(t, peer, server)

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
	waitForConns(t, c, 0, "a server/error in place of server/init did not end the handshake")

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
	handled(t, peer, server)

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

func rolelessPeer(t *testing.T, c *Client, ln net.Listener) (*wsPeer, *serverSide) {
	t.Helper()
	peer, server := bringUpWith(t, c, ln, serverPlan{psk: SentinelPSK(), cat: categorySentinel})
	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{},
		ActiveRoles: &none,
	}))
	handled(t, peer, server)
	return peer, server
}

func fillTo(t *testing.T, c *Client, ln net.Listener, want int) {
	t.Helper()
	for n := c.conns(); n < want; n = c.conns() {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { nc.Close() })
		waitFor(t, "the connection to be accepted", func() bool { return c.conns() > n })
	}
}

func TestARolelessSessionKeepsItsConnectionAndIsKeptAlive(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.provisionalAfter = 200 * time.Millisecond
	c.pingEvery = 100 * time.Millisecond
	serveOn(t, c, ln)
	peer, server := rolelessPeer(t, c, ln)

	time.Sleep(500 * time.Millisecond)
	handled(t, peer, server)
	if c.conns() != 1 {
		t.Errorf("conns = %d, want 1: a server that activated with no role may stay,"+
			" and closing it makes music assistant redial every 30 seconds", c.conns())
	}
	if err := peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if op, _ := peer.read(); op != opPing {
		t.Errorf("the Dot sent %#x, want a ping: nothing would notice the peer going", op)
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

type peerAt struct {
	peer   *wsPeer
	server *serverSide
}

func rolelessPeers(t *testing.T, c *Client, ln net.Listener, n int) []peerAt {
	t.Helper()
	var out []peerAt
	for range n {
		peer, server := rolelessPeer(t, c, ln)
		out = append(out, peerAt{peer, server})
	}
	return out
}

func activateNothing(t *testing.T, peer *wsPeer, server *serverSide) {
	t.Helper()
	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{},
		ActiveRoles: &none,
	}))
}

func toldGoodbye(t *testing.T, peer *wsPeer, server *serverSide, want string) {
	t.Helper()
	kind, payload := nextJSON(t, peer, server)
	var bye goodbye
	if err := json.Unmarshal(payload, &bye); err != nil {
		t.Fatalf("goodbye: %v", err)
	}
	if kind != typeClientGoodbye || bye.Reason != want {
		t.Errorf("got %s %q, want %s %q", kind, bye.Reason, typeClientGoodbye, want)
	}
}

func TestABareSocketDisplacesNobody(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server := rolelessPeer(t, c, ln)
	fillTo(t, c, ln, maxConns)

	extra, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer extra.Close()
	if err := extra.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := extra.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("a full Dot left the socket past its cap open")
	}
	handled(t, peer, server)
	if n := c.conns(); n != maxConns {
		t.Errorf("conns = %d, want %d: a socket that never activated displaced a server", n,
			maxConns)
	}
}

func TestAPlaybackActivationAtTheCapDisplacesEveryIdleServer(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	idle := rolelessPeers(t, c, ln, maxAdmitted)

	bringUp(t, c, ln)
	for _, p := range idle {
		toldGoodbye(t, p.peer, p.server, goodbyeAnotherServer)
	}
	c.mu.Lock()
	held, admitted := c.held, len(c.admitted)
	c.mu.Unlock()
	if held == nil || admitted != 1 {
		t.Errorf("held = %v, admitted = %d; the server that outranked them should be the"+
			" one left", held != nil, admitted)
	}
}

func TestAnIdleActivationAtTheCapIsRefusedUnlessItPlayedLast(t *testing.T) {
	for _, last := range []bool{false, true} {
		ln := listenLocal(t)
		c := testClient(t)
		c.Last = &LastPlayback{}
		serveOn(t, c, ln)
		idle := rolelessPeers(t, c, ln, maxAdmitted)

		peer, server := bringUpWith(t, c, ln, serverPlan{psk: SentinelPSK(), cat: categorySentinel})
		if last {
			if err := c.Last.set(server.pair.Public); err != nil {
				t.Fatal(err)
			}
		}
		activateNothing(t, peer, server)
		if !last {
			toldGoodbye(t, peer, server, goodbyeConcurrent)
			for _, p := range idle {
				handled(t, p.peer, p.server)
			}
			continue
		}
		for _, p := range idle {
			toldGoodbye(t, p.peer, p.server, goodbyeAnotherServer)
		}
		handled(t, peer, server)
	}
}

func TestANewcomerPlaysAfterAHandover(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	player.single = true
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })

	peer, server, _ = bringUp(t, c, ln)
	playing(t, peer, server)
	waitFor(t, "the newcomer's stream to be opened", func() bool { return player.count() == 2 })
}

func TestADisplacedSessionCannotOpenAStream(t *testing.T) {
	var gate streamGate
	first := &fakeStream{}
	if got, err := gate.open(func() (Stream, error) { return first, nil }); got != first || err != nil {
		t.Fatalf("the gate gave %v, %v; want the stream it opened", got, err)
	}
	gate.close()
	if first.shut() != 1 {
		t.Errorf("closing the gate closed its stream %d times, want 1", first.shut())
	}
	opened := false
	got, err := gate.open(func() (Stream, error) { opened = true; return &fakeStream{}, nil })
	if opened || got != nil || err != nil {
		t.Errorf("a closed gate opened a stream (%v, %v, %v): a server displaced while it"+
			" read a stream/start holds the player against the newcomer", opened, got, err)
	}
}

func TestTheServerThatLastPlayedIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last")
	last, err := LoadLastPlayback(path)
	if err != nil {
		t.Fatal(err)
	}
	ln := listenLocal(t)
	c := testClient(t)
	c.Last = last
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	handled(t, peer, server)

	again, err := LoadLastPlayback(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.id(); got != EncodeID(server.pair.Public) {
		t.Errorf("the last server to play reloaded as %q, want %q", got,
			EncodeID(server.pair.Public))
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file is %v, %v; want 0600", info, err)
	}
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
	waitFor(t, "the handshake line to be written, or nothing here is tested", func() bool {
		return strings.Contains(out.String(), "handshake with")
	})

	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 300 {
			t.Errorf("a peer's own name stretched a log line to %d bytes", len(line))
		}
		if strings.HasPrefix(line, "sendspin: forged") {
			t.Error("a newline in the server's name forged a log line")
		}
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
	handled(t, peer, server)

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
	handled(t, peer, server)

	if spent := c.Peer.Written() - before; spent > 6 {
		t.Errorf("1000 ordinary messages spent %d lines of the peer budget; one per"+
			" distinct thing said is the most that says anything new", spent)
	}
	if !strings.Contains(out.String(), "not handled yet") {
		t.Error("the first unhandled message was never reported, so nothing was tested")
	}
}

func TestALeavingSessionDoesNotReleaseAnotherServersHold(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	first, firstServer, _ := bringUp(t, c, ln)
	second, secondServer, _ := bringUp(t, c, ln)
	if kind, _ := nextJSON(t, first, firstServer); kind != typeClientGoodbye {
		t.Fatalf("the first server got %s, want %s", kind, typeClientGoodbye)
	}
	first.conn.Close()
	waitForConns(t, c, 1, "the displaced server's connection was never torn down")
	handled(t, second, secondServer)

	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if held == nil {
		t.Error("the displaced server's disconnect released the hold the second server took")
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
	handled(t, peer, server)

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

func TestRepeatingAnActivationIsNotAnsweredAgain(t *testing.T) {
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

	probe := 901
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&probe)))
	if got := delaySet(t, peer, server); got != probe {
		t.Errorf("a repeated activation was answered with a %d ms client/state before"+
			" the %d ms sent after it: each one writes another client/state and leaves"+
			" another keepalive ticker running", got, probe)
	}
	if _, err := peer.conn.Write(frame(true, opPing, []byte("handled"))); err != nil {
		t.Fatalf("writing a ping: %v", err)
	}
	for {
		op, body := peer.read()
		if op == opPong && string(body) == "handled" {
			return
		}
		if op != opBinary {
			continue
		}
		if kind, plain := server.open(t, body); kind == msgJSON {
			var env envelope
			if err := json.Unmarshal(plain, &env); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if env.Type == typeClientState {
				t.Fatal("a second client/state followed the one the delay asked for, so a" +
					" repeated activation was answered again")
			}
		}
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
	handled(t, peer, server)

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
	handled(t, peer, server)
	peer.conn.Close()
	waitForConns(t, c, 0, "the dropped connection was never torn down")

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

	bad := make([]byte, 1+chunkHeadBytes+2)
	bad[0] = binaryAudioChunk
	binary.BigEndian.PutUint64(bad[1:9], uint64(stampCeiling+1))
	peer.writeBinary(server.seal(t, bad))
	peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 3}))
	handled(t, peer, server)

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
	playingAt(t, peer, server, StreamRate)
}

func playingAt(t *testing.T, peer *wsPeer, server *serverSide, rate int) {
	t.Helper()
	peer.writeBinary(server.sealJSON(t, typeStreamStart, streamStart{
		ServerTransmitted: 1, Player: &streamPlayer{
			Codec: codecPCM, SampleRate: rate,
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
	handled(t, peer, server)

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

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities: []string{}, ActiveRoles: &none}))
	handled(t, peer, server)

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
	b := make([]byte, 1+chunkHeadBytes+2)
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
	peer.writeBinary(server.seal(t, []byte{0x40, 1, 2}))
	handled(t, peer, server)

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
	handled(t, peer, server)

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
	handled(t, peer, server)
	time.Sleep(2 * c.reportEvery)

	if strings.Contains(out.String(), "1 chunks") {
		t.Fatal("a summary arrived with no message to carry it, so nothing here is tested")
	}
	peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{
		PlaybackState: "playing", GroupID: "g1", GroupName: "kitchen"}))
	handled(t, peer, server)

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
	handled(t, peer, server)

	if spent := c.Peer.Written() - before; spent > 2 {
		t.Errorf("a server flapping its stream spent %d lines of the budget the roles,"+
			" the clock and the handshake share, and that budget never refills", spent)
	}
	if c.Play.Written() == 0 {
		t.Error("the streams were not logged at all")
	}
}

func admittedAt(t *testing.T, c *Client, id string, r rank) (net.Conn, *Session) {
	t.Helper()
	nc, far := net.Pipe()
	t.Cleanup(func() { nc.Close(); far.Close() })
	s := &Session{serverID: id}
	c.mu.Lock()
	if c.live == nil {
		c.live = map[net.Conn]*Session{}
	}
	c.live[nc] = s
	c.mu.Unlock()
	if r != rankEmpty || id != "" {
		if _, err := c.admit(nc, s, r, false, false); err != nil {
			t.Fatalf("admitting %q: %v", id, err)
		}
	}
	return nc, s
}

func TestAServerThatIsPairingIsNotDisplaced(t *testing.T) {
	c := testClient(t)
	pairing, _ := admittedAt(t, c, "pairing", rankPairing)
	for i := range maxAdmitted - 1 {
		admittedAt(t, c, fmt.Sprint("idle ", i), rankEmpty)
	}
	nc, s := admittedAt(t, c, "", rankEmpty)
	out, err := c.admit(nc, s, rankPlayback, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out[pairing]; ok || len(out) != maxAdmitted-1 {
		t.Errorf("displaced %d servers, the pairing one among them: %v; only the idle ones"+
			" rank below playback", len(out), ok)
	}
}

func TestADisplacedServerCannotTakeThePlayer(t *testing.T) {
	c := testClient(t)
	var idle []*Session
	for i := range maxAdmitted {
		_, s := admittedAt(t, c, fmt.Sprint("idle ", i), rankEmpty)
		idle = append(idle, s)
	}
	nc, s := admittedAt(t, c, "", rankEmpty)
	if _, err := c.admit(nc, s, rankPlayback, false, false); err != nil {
		t.Fatal(err)
	}
	if err := c.hold(idle[0], true); !errors.Is(err, errBusy) {
		t.Errorf("a displaced server took the player: %v", err)
	}
}

func TestAnIdleNewcomerDoesNotDisplaceTheSameLastServer(t *testing.T) {
	key := bytes.Repeat([]byte{7}, keyLen)
	c := testClient(t)
	c.Last = &LastPlayback{server: key}
	admittedAt(t, c, EncodeID(key), rankEmpty)
	for i := range maxAdmitted - 1 {
		admittedAt(t, c, fmt.Sprint("idle ", i), rankEmpty)
	}
	nc, s := admittedAt(t, c, "", rankEmpty)
	s.serverID = EncodeID(key)
	if _, err := c.admit(nc, s, rankEmpty, false, false); !errors.Is(err, errBusy) {
		t.Errorf("an idle newcomer displaced an idle server with the same last-playback id: %v",
			err)
	}
}

func TestHoldingThePlayerRanksAsPlayback(t *testing.T) {
	if got := rankOf(nil, []string{rolePlayerV1}); got != rankPlayback {
		t.Errorf("a server holding the player with no activities ranks %d, want %d: an idle"+
			" newcomer would displace it", got, rankPlayback)
	}
}

func TestOnlyOneServerAtATimeIsAdmittedForPlayback(t *testing.T) {
	c := testClient(t)
	for i := range maxAdmitted {
		nc, s := admittedAt(t, c, "", rankEmpty)
		s.serverID = fmt.Sprint("playback ", i)
		out, err := c.admit(nc, s, rankPlayback, false, false)
		if err != nil {
			t.Fatalf("playback %d: %v", i, err)
		}
		if i > 0 && len(out) != 1 {
			t.Fatalf("playback %d displaced %d servers, want the 1 before it", i, len(out))
		}
	}
	c.mu.Lock()
	n := len(c.admitted)
	c.mu.Unlock()
	if n != 1 {
		t.Errorf("admitted = %d, want 1: 7 servers declaring playback with no role filled"+
			" every slot, and music assistant would get concurrent_attempt", n)
	}
}

func TestANewcomerThatOutranksNoOneAtTheCapIsRefused(t *testing.T) {
	c := testClient(t)
	for i := range maxAdmitted {
		admittedAt(t, c, fmt.Sprint("pairing ", i), rankPairing)
	}
	nc, s := admittedAt(t, c, "", rankEmpty)
	if _, err := c.admit(nc, s, rankPlayback, false, false); !errors.Is(err, errBusy) {
		t.Errorf("an eighth server was admitted with no idle server to displace: %v", err)
	}
}

func TestAnAdmittedServerActivatingAgainAtTheCapIsNotRanked(t *testing.T) {
	c := testClient(t)
	nc, s := admittedAt(t, c, "again", rankEmpty)
	for i := range maxAdmitted - 1 {
		admittedAt(t, c, fmt.Sprint("idle ", i), rankEmpty)
	}
	out, err := c.admit(nc, s, rankEmpty, false, false)
	if err != nil || len(out) != 0 {
		t.Errorf("an admitted server activating again got %v and displaced %d", err, len(out))
	}
}

func TestADisplacedServerIsNotAdmittedAgain(t *testing.T) {
	c := testClient(t)
	nc, s := admittedAt(t, c, "", rankEmpty)
	s.displaced.Store(true)
	if _, err := c.admit(nc, s, rankEmpty, false, false); !errors.Is(err, errBusy) {
		t.Errorf("a displaced server was admitted: %v", err)
	}
}

func TestPairingRanksAboveNothing(t *testing.T) {
	if got := rankOf([]string{activityPairing}, nil); got != rankPairing {
		t.Errorf("a pairing server ranks %d, want %d", got, rankPairing)
	}
}

func TestALeavingServerIsNoLongerAdmitted(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, _ := rolelessPeer(t, c, ln)
	peer.conn.Close()
	waitForConns(t, c, 0, "the server that hung up kept its connection")
	c.mu.Lock()
	n := len(c.admitted)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("admitted = %d after the server left, want 0: stale entries fill the cap", n)
	}
}

func TestAnActivationAnsweredWithPairAbortAdmitsNothing(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.provisionalAfter = 300 * time.Millisecond
	serveOn(t, c, ln)
	peer, server := bringUpWith(t, c, ln, serverPlan{psk: SentinelPSK(), cat: categorySentinel})
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPairing},
		ActiveRoles: &[]string{},
		Pairing:     &activatePairing{Method: methodPairingPSK},
	}))
	if kind, _ := nextJSON(t, peer, server); kind != typePairAbort {
		t.Fatalf("got %s, want %s", kind, typePairAbort)
	}
	c.mu.Lock()
	n := len(c.admitted)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("admitted = %d; the spec says not to arbitrate an activation that is not"+
			" admissible", n)
	}
	waitForConns(t, c, 0, "the activation lifted the provisional deadline")
}

func TestAnActivationWithoutPlaybackIsNotTheLastToPlay(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.Last = &LastPlayback{}
	serveOn(t, c, ln)
	rolelessPeer(t, c, ln)
	if got := c.Last.id(); got != "" {
		t.Errorf("a server that declared nothing became the last to play: %q", got)
	}
}

func TestAPlaybackNewcomerTakesThePlayerFromItsHolder(t *testing.T) {
	c := testClient(t)
	holderConn, holder := admittedAt(t, c, "", rankEmpty)
	holder.serverID = "music assistant"
	if _, err := c.admit(holderConn, holder, rankPlayback, true, true); err != nil {
		t.Fatal(err)
	}
	nc, s := admittedAt(t, c, "", rankEmpty)
	s.serverID = "newcomer"
	out, err := c.admit(nc, s, rankPlayback, true, false)
	if _, ok := out[holderConn]; err != nil || !ok {
		t.Fatalf("the newcomer got %v and did not displace the holder", err)
	}
	if !holder.displaced.Load() || c.held != s {
		t.Error("the player did not pass to the newcomer")
	}

	other, roleless := admittedAt(t, c, "", rankEmpty)
	roleless.serverID = "roleless"
	if _, err := c.admit(other, roleless, rankPlayback, false, false); err != nil {
		t.Fatal(err)
	}
	if c.held != nil {
		t.Error("a holder displaced by a server taking no role is still reported as holding")
	}
}

func TestAPlaybackNewcomerAtTheCapDisplacesOnlyThePlaybackServer(t *testing.T) {
	c := testClient(t)
	playing, _ := admittedAt(t, c, "playback", rankPlayback)
	for i := range maxAdmitted - 1 {
		admittedAt(t, c, fmt.Sprint("idle ", i), rankEmpty)
	}
	nc, s := admittedAt(t, c, "", rankEmpty)
	s.serverID = "newcomer"
	out, err := c.admit(nc, s, rankPlayback, false, false)
	if _, ok := out[playing]; err != nil || !ok || len(out) != 1 {
		t.Errorf("got %v and displaced %d servers: displacing the playback server frees"+
			" the slot, so the idle ones stay", err, len(out))
	}
}

func TestAPairingNewcomerDoesNotDisplaceAPlaybackServer(t *testing.T) {
	c := testClient(t)
	playing, _ := admittedAt(t, c, "playback", rankPlayback)
	nc, s := admittedAt(t, c, "", rankEmpty)
	s.serverID = "pairing"
	out, err := c.admit(nc, s, rankPairing, false, false)
	if _, ok := out[playing]; err != nil || ok {
		t.Errorf("a pairing activation displaced a playback server: %v", err)
	}
}
