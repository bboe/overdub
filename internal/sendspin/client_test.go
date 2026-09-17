package sendspin

import (
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func serveOn(t *testing.T, c *Client, ln net.Listener) {
	t.Helper()
	go func() { _ = c.Serve(ln) }()
	t.Cleanup(func() {
		ln.Close()
		deadline := time.Now().Add(5 * time.Second)
		for {
			n := c.conns()
			if n == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d connections were still being served when the test ended", n)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func dialLocal(t *testing.T, ln net.Listener) *wsPeer {
	t.Helper()
	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := nc.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	t.Cleanup(func() { nc.Close() })
	return newWSPeer(t, nc)
}

func nextJSON(t *testing.T, peer *wsPeer, server *serverSide) (string, json.RawMessage) {
	t.Helper()
	for {
		kind, payload := readJSON(t, peer, server)
		if kind != typeClientTime {
			return kind, payload
		}
	}
}

func readJSON(t *testing.T, peer *wsPeer, server *serverSide) (string, json.RawMessage) {
	t.Helper()
	_, sealed := peer.read()
	kind, body := server.open(t, sealed)
	if kind != msgJSON {
		t.Fatalf("wanted a JSON message, got type %#x", kind)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return env.Type, env.Payload
}

func bringUp(t *testing.T, c *Client, ln net.Listener) (*wsPeer, *serverSide, clientState) {
	t.Helper()
	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))

	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("wanted %s, got %s", typeClientState, kind)
	}
	var cs clientState
	if err := json.Unmarshal(payload, &cs); err != nil {
		t.Fatalf("decoding client/state: %v", err)
	}
	if kind, _ := readJSON(t, peer, server); kind != typeClientTime {
		t.Fatalf("wanted %s once the player role is active, got %s", typeClientTime, kind)
	}
	return peer, server, cs
}

func testClient(t *testing.T) *Client {
	t.Helper()
	keys := testKeys(t)
	return &Client{
		Config:      testConfig(),
		Keys:        keys,
		PSKs:        PSKSet{Pairing: keys.PairingPSK},
		MinBufferMS: 500,
		timeEvery:   10 * time.Second,
	}
}

func TestServeTakesAConnectionThroughToClientState(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	_, _, state := bringUp(t, c, ln)
	if state.Available {
		t.Error("reported available before there is any clock sync or audio")
	}
	if state.Player == nil {
		t.Fatal("client/state carried no player object while the player role is active")
	}
	if state.Player.MinBufferMS != 500 {
		t.Errorf("min_buffer_ms = %d, want 500", state.Player.MinBufferMS)
	}
	if state.Player.SupportedCommands == nil {
		t.Error("supported_commands must be present, empty when no command is accepted")
	}
	if state.Player.StaticDelayMS != 0 {
		t.Errorf("static_delay_ms = %d, want 0: it is the delay *past* this device's audio"+
			" port, which a Dot with one speaker does not have, and the server sends"+
			" that much earlier for it. The delay inside the Dot is ours to compensate"+
			" for by asking the player where its audio has reached, not to declare here",
			state.Player.StaticDelayMS)
	}
}

func TestServeRefusesASecondServerWhileOneIsHeld(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	_, _, _ = bringUp(t, c, ln)

	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "second server"}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientGoodbye {
		t.Fatalf("second server got %s, want %s", kind, typeClientGoodbye)
	}
	if !strings.Contains(string(payload), goodbyeConcurrent) {
		t.Errorf("goodbye reason = %s, want %s", payload, goodbyeConcurrent)
	}
}

func TestServeAdmitsANewServerAfterTheFirstGoesAway(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	peer, _, _ := bringUp(t, c, ln)
	peer.conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		held := c.held
		c.mu.Unlock()
		if held == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was never released after the server went away")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, state := bringUp(t, c, ln); state.Player == nil {
		t.Error("the second server was not brought up")
	}
}

func TestRecordsCarryThePath(t *testing.T) {
	got := Advert("kitchen").Records
	if len(got) == 0 || got[0] != "path="+Path {
		t.Errorf("records = %v, want path= first and required", got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "name=kitchen") {
		t.Errorf("records = %v, want the client name", got)
	}
}

func TestTheAdvertChangesOnlyWhenTheDeviceReboots(t *testing.T) {
	tokenOf := func(records []string) string {
		for _, r := range records {
			if after, ok := strings.CutPrefix(r, "overdub_boot="); ok {
				return after
			}
		}
		return ""
	}
	if _, err := os.Stat(bootIDPath); err == nil {
		first := Advert("kitchen").Records
		token := tokenOf(first)
		if tokenOf(Advert("kitchen").Records) != token {
			t.Error("the token moved inside one run, so a re-announcement would look" +
				" like a restart")
		}
		if instance != bootToken(bootIDPath) {
			t.Error("the advertised token is not the one the kernel's boot id gives," +
				" so it does not hold still for a boot")
		}
		if len(token) != 8 {
			t.Fatalf("boot token = %q, want 8 hex characters", token)
		}
		if _, err := hex.DecodeString(token); err != nil {
			t.Errorf("boot token = %q, want hex: %v", token, err)
		}
	}

	dir := t.TempDir()
	one := filepath.Join(dir, "boot-one")
	two := filepath.Join(dir, "boot-two")
	if err := os.WriteFile(one, []byte("21050ace-47d1-4e34-a0b9-091873d7cc3d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("7c9e6679-7425-40de-944b-e07fc1f90ae7\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if bootToken(one) != bootToken(one) {
		t.Error("the token moved without a reboot, so a crash loop would announce a" +
			" change on every respawn")
	}
	if bootToken(one) == bootToken(two) {
		t.Error("two boots produced the same token, so a restart discovery has to see" +
			" would look like a refresh")
	}

	missing := filepath.Join(dir, "absent")
	if got := bootToken(missing); got != "" {
		t.Errorf("bootToken without a boot id = %q, want empty: a value of its own would"+
			" move on every respawn, which is the announcement storm the boot id avoids",
			got)
	}

	saved := instance
	instance = ""
	bare := Advert("kitchen").Records
	instance = saved
	if tokenOf(bare) != "" {
		t.Errorf("advertised a boot record with no token to put in it: %v", bare)
	}
	if len(bare) != 2 {
		t.Errorf("advert carries %d records without a token, want the path and the name",
			len(bare))
	}
}

func TestBufferCapacityHoldsWholeSeconds(t *testing.T) {
	perSecond := StreamRate * StreamChannels * (StreamBitDepth / 8)
	if BufferCapacity != perSecond*bufferSeconds {
		t.Errorf("buffer capacity = %d, want %d", BufferCapacity, perSecond*bufferSeconds)
	}
	if BufferCapacity < perSecond {
		t.Error("buffer capacity holds less than a second of audio")
	}
}

func TestClientStateIsExactlyThisOnTheWire(t *testing.T) {
	got, err := marshalEnvelope(typeClientState, clientState{
		Player: &playerState{MinBufferMS: 500, SupportedCommands: []string{}},
	})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	const want = `{"type":"client/state","payload":{"available":false,` +
		`"player":{"static_delay_ms":0,"required_lead_time_ms":0,` +
		`"min_buffer_ms":500,"supported_commands":[]}}}`
	if string(got) != want {
		t.Errorf("client/state is\n%s\nwant\n%s", got, want)
	}
}

func TestTheServiceAndItsWindowsAreFixed(t *testing.T) {
	if Service != "_sendspin._tcp.local." {
		t.Errorf("service = %q", Service)
	}
	if Path != "/sendspin" {
		t.Errorf("path = %q", Path)
	}
	if Port != 8928 {
		t.Errorf("port = %d, want 8928", Port)
	}
	if maxConns != 8 {
		t.Errorf("connection cap = %d, want 8; the test that proves the cap works reads"+
			" this same constant, so nothing else notices it moving", maxConns)
	}
	if bufferSeconds != 2 {
		t.Errorf("buffer seconds = %d, want 2", bufferSeconds)
	}
	if idleWait <= pingAfter {
		t.Errorf("idle window %s does not outlast the %s ping interval", idleWait, pingAfter)
	}
	if handshakeWait != 30*time.Second || provisionalWait != 30*time.Second {
		t.Errorf("windows are %s and %s, want 30s and 30s", handshakeWait, provisionalWait)
	}
}

func TestSwitchingOffWhileAPeerIsMidHandshakeDoesNotPanic(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()

	deadline := time.Now().Add(5 * time.Second)
	for c.conns() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection was never tracked, so the nil session is not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}

	c.Close()
}

func TestTheGoodbyeIsBoundedByItsOwnDeadlineNotTheIdleOne(t *testing.T) {
	session, _, _ := pairedSession(t)
	session.ws.setIdle(3 * time.Second)

	c := &Client{Config: testConfig(), goodbyeAfter: 100 * time.Millisecond}
	nc := session.ws.c
	if err := c.enter(nc); err != nil {
		t.Fatalf("enter: %v", err)
	}
	c.track(nc, session)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		c.Close()
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		if took > time.Second {
			t.Errorf("Close took %v against a goodbye budget of 100ms, so it waited on"+
				" the idle deadline instead", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Close never returned inside the idle window, so the goodbye is bound" +
			" by the idle deadline rather than its own")
	}
}

func TestTheGoodbyeIsBoundedEvenWhenAWriteIsAlreadyBlocked(t *testing.T) {
	session, _, _ := pairedSession(t)
	session.ws.setIdle(3 * time.Second)

	c := &Client{Config: testConfig(), goodbyeAfter: 100 * time.Millisecond}
	nc := session.ws.c
	if err := c.enter(nc); err != nil {
		t.Fatalf("enter: %v", err)
	}
	c.track(nc, session)

	started := make(chan struct{})
	go func() {
		close(started)
		_ = session.Goodbye(goodbyeShutdown)
	}()
	<-started
	time.Sleep(100 * time.Millisecond)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		c.Close()
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		if took > time.Second {
			t.Errorf("Close took %v against a goodbye budget of 100ms, so it waited on"+
				" the deadline the blocked write had already armed", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Close never returned: a write already blocked holds the session mutex" +
			" under the idle deadline, which narrowing the window cannot reach")
	}
}

func TestTheGoodbyesOfManyStalledSessionsCostOneBudgetBetweenThem(t *testing.T) {
	const sessions = 4
	c := &Client{Config: testConfig(), goodbyeAfter: 300 * time.Millisecond}
	for i := 0; i < sessions; i++ {
		session, _, _ := pairedSession(t)
		nc := session.ws.c
		if err := c.enter(nc); err != nil {
			t.Fatalf("enter %d: %v", i, err)
		}
		c.track(nc, session)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		c.Close()
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		if took > time.Second {
			t.Errorf("Close took %v for %d stalled sessions against a 300ms budget, so"+
				" the goodbyes are spent one after another rather than together",
				took, sessions)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close never returned for four stalled sessions")
	}
}

type deafConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func newDeafConn(inner net.Conn) *deafConn {
	return &deafConn{Conn: inner, closed: make(chan struct{})}
}

func (d *deafConn) Write([]byte) (int, error) {
	<-d.closed
	return 0, net.ErrClosed
}

func (d *deafConn) SetWriteDeadline(time.Time) error { return nil }

func (d *deafConn) SetDeadline(time.Time) error { return nil }

func (d *deafConn) Close() error {
	d.once.Do(func() { close(d.closed) })
	return d.Conn.Close()
}

func TestCloseGivesUpOnASessionWhoseDeadlineSomethingElseRearmed(t *testing.T) {
	session, _, _ := pairedSession(t)
	deaf := newDeafConn(session.ws.c)
	session.ws.c = deaf

	c := &Client{Config: testConfig(), goodbyeAfter: 150 * time.Millisecond}
	if err := c.enter(deaf); err != nil {
		t.Fatalf("enter: %v", err)
	}
	c.track(deaf, session)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		c.Close()
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		if took > 2*time.Second {
			t.Errorf("Close took %v against a 150ms budget, so a deadline armed by"+
				" something else outlasted it", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned: the goodbye is bounded by a deadline that" +
			" anything else on the connection may re-arm")
	}
}
