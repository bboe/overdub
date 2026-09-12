package sendspin

import (
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// serveOn starts the client and, when the test ends, waits for its per-connection
// goroutines to finish. They write to the process-wide log, so one left running
// lands its lines in whichever later test is counting them -- measured as a 1-in-40
// failure under -shuffle before this waited.
func serveOn(t *testing.T, c *Client, ln net.Listener) {
	t.Helper()
	go func() { _ = c.Serve(ln) }()
	t.Cleanup(func() {
		ln.Close()
		deadline := time.Now().Add(5 * time.Second)
		for {
			c.mu.Lock()
			n := c.conns
			c.mu.Unlock()
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

// nextJSON reads the next frame and decrypts it. Every message must go through
// this in order: the AEAD nonce advances per message, so skipping one makes
// every later decrypt fail as an authentication error.
func nextJSON(t *testing.T, peer *wsPeer, server *serverSide) (string, json.RawMessage) {
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

// bringUp takes one connection all the way to an activated player role and
// returns the server side plus the client/state the Dot reported.
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
	// A server that stopped retrying only comes back when discovery hands it a
	// change. python-zeroconf, which Music Assistant browses through, drops any
	// announcement whose records it already holds -- a device that died without a
	// goodbye keeps its records cached for the PTR TTL, so an identical advert on
	// return is invisible. The boot token is what makes the return a change, and
	// it has to hold still within a boot: a crash loop respawning every five
	// seconds would otherwise announce a change on each one.
	tokenOf := func(records []string) string {
		for _, r := range records {
			if after, ok := strings.CutPrefix(r, "overdub_boot="); ok {
				return after
			}
		}
		return ""
	}
	// The daemon runs on Linux, where the boot id is always there. -race has no arm
	// build so it runs on the host, which may not be Linux at all, and there the
	// advert correctly carries no token -- so what the live advert says is only
	// checkable where the kernel supplies one.
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

	// With no token there is nothing to say, and the record has to be left out rather
	// than sent empty: a server reading one would take it as a boot of its own.
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
	// static_delay_ms is one of the three field names aiosendspin 9.1.1 disagrees
	// with the spec about, and sending the spec's name is tolerated in silence --
	// so a literal is the only thing that notices it changing.
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
	// The ping has to fall inside the idle window, or every quiet connection is cut
	// before the keepalive that would have held it open.
	if idleWait <= pingAfter {
		t.Errorf("idle window %s does not outlast the %s ping interval", idleWait, pingAfter)
	}
	if handshakeWait != 30*time.Second || provisionalWait != 30*time.Second {
		t.Errorf("windows are %s and %s, want 30s and 30s", handshakeWait, provisionalWait)
	}
}
