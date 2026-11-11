package sendspin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flynn/noise"
)

func testRecords(t *testing.T) *Records {
	t.Helper()
	r, err := LoadRecords(filepath.Join(t.TempDir(), "pairs"))
	if err != nil {
		t.Fatalf("LoadRecords: %v", err)
	}
	return r
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k, err := NewPSK()
	if err != nil {
		t.Fatalf("NewPSK: %v", err)
	}
	return k
}

func serverKeypair(t *testing.T) noise.DHKey {
	t.Helper()
	pair, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	return pair
}

func (v *serverSide) rekey(t *testing.T, peer *wsPeer, client []byte, psk []byte, cat category) {
	t.Helper()
	hs := v.sendRekey(t, peer, client, psk, cat)
	for {
		kind, payload := readJSON(t, peer, v)
		if kind != typeNoiseHandshake {
			continue
		}
		var nf noiseFrame
		if err := json.Unmarshal(payload, &nf); err != nil {
			t.Fatalf("decoding noise message 2: %v", err)
		}
		second, err := base64.RawURLEncoding.DecodeString(nf.Data)
		if err != nil {
			t.Fatalf("decoding noise message 2 data: %v", err)
		}
		_, cs1, cs2, err := hs.ReadMessage(nil, second)
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		v.send, v.recv, v.hash = cs1, cs2, hs.ChannelBinding()
		return
	}
}

func (v *serverSide) sendRekey(t *testing.T, peer *wsPeer, client []byte, psk []byte,
	cat category) *noise.HandshakeState {

	t.Helper()
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeKK,
		Initiator:             true,
		PresharedKeyPlacement: 2,
		StaticKeypair:         v.pair,
		PeerStatic:            client,
		Prologue:              v.hash,
	})
	if err != nil {
		t.Fatalf("NewHandshakeState: %v", err)
	}
	if err := hs.SetPresharedKey(psk); err != nil {
		t.Fatalf("SetPresharedKey: %v", err)
	}
	intro, err := json.Marshal(handshakeIntro{PSKID: PSKID(psk), Category: cat})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	first, _, _, err := hs.WriteMessage(nil, intro)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	frame, err := noiseEnvelope(first)
	if err != nil {
		t.Fatalf("noiseEnvelope: %v", err)
	}
	peer.writeBinary(v.seal(t, append([]byte{msgJSON}, frame...)))
	return hs
}

func TestRecordsSurviveAReload(t *testing.T) {
	r := testRecords(t)
	server, psk := randomKey(t), randomKey(t)
	if err := r.put(server, psk, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	again, err := LoadRecords(r.path)
	if err != nil {
		t.Fatalf("LoadRecords: %v", err)
	}
	rec, ok := again.find(PSKID(psk))
	if !ok || !bytes.Equal(rec.server, server) || !bytes.Equal(rec.psk, psk) {
		t.Fatalf("reloaded record = %x %x, %v; want %x %x", rec.server, rec.psk, ok, server, psk)
	}
	info, err := os.Stat(r.path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != keyFileMode {
		t.Errorf("the record file is %#o, want %#o", info.Mode().Perm(), keyFileMode)
	}
}

func TestRecordsRefuseAFileOthersCanRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairs")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadRecords(path); err == nil || !strings.Contains(err.Error(), "long-term") {
		t.Fatalf("LoadRecords = %v, want a refusal naming the long-term psks", err)
	}
}

func TestRecordsRefuseAPartialRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairs")
	if err := os.WriteFile(path, make([]byte, recordLen+1), keyFileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadRecords(path); err == nil {
		t.Fatal("a file one byte past a record loaded")
	}
}

func TestANewPairingReplacesTheServersOldRecord(t *testing.T) {
	r := testRecords(t)
	server, old, fresh := randomKey(t), randomKey(t), randomKey(t)
	if err := r.put(server, old, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := r.put(server, fresh, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, ok := r.find(PSKID(old)); ok {
		t.Error("the server's old record survived its new pairing")
	}
	if r.Len() != 1 {
		t.Errorf("%d records, want 1", r.Len())
	}
}

func TestAPairingAtCapacityEvictsTheOldestUnprotectedRecord(t *testing.T) {
	r := testRecords(t)
	var psks [][]byte
	for range recordCapacity {
		psk := randomKey(t)
		psks = append(psks, psk)
		if err := r.put(randomKey(t), psk, nil); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	protected := map[string]bool{PSKID(psks[0]): true}
	fresh := randomKey(t)
	if err := r.put(randomKey(t), fresh, protected); err != nil {
		t.Fatalf("put: %v", err)
	}
	if r.Len() != recordCapacity {
		t.Errorf("%d records, want %d", r.Len(), recordCapacity)
	}
	for _, c := range []struct {
		name string
		psk  []byte
		want bool
	}{
		{"the protected oldest", psks[0], true},
		{"the oldest unprotected", psks[1], false},
		{"the new one", fresh, true},
	} {
		if _, ok := r.find(PSKID(c.psk)); ok != c.want {
			t.Errorf("%s: kept = %v, want %v", c.name, ok, c.want)
		}
	}
}

func TestTheRecordCapacityLeavesOneToEvict(t *testing.T) {
	if recordCapacity <= maxConns || recordCapacity < 5 {
		t.Errorf("record capacity %d with %d connections: the spec needs at least 5, and"+
			" more than the connections that can hold one open", recordCapacity, maxConns)
	}
}

func TestALongTermPSKBindsItsServer(t *testing.T) {
	r := testRecords(t)
	server, psk := randomKey(t), randomKey(t)
	if err := r.put(server, psk, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	set := PSKSet{Records: r}
	got, cat, err := set.selectPSK(PSKID(psk), categoryLongTerm, EncodeID(server), true)
	if err != nil || cat != categoryLongTerm || !bytes.Equal(got, psk) {
		t.Fatalf("selectPSK = %x, %q, %v; want the record", got, cat, err)
	}
	if _, _, err := set.selectPSK(PSKID(psk), categoryLongTerm, EncodeID(randomKey(t)),
		true); err == nil {
		t.Error("another server's long-term psk matched: a misbinding must fail, not fall back")
	}
}

func TestARehandshakeMissDoesNotFallBack(t *testing.T) {
	if _, _, err := (PSKSet{}).selectPSK(PSKID(randomKey(t)), categoryLongTerm, "", false); err == nil {
		t.Error("a miss without the fallback matched something")
	}
}

func pairOver(t *testing.T, c *Client, ln net.Listener, pair noise.DHKey) (*wsPeer, *serverSide, []byte) {
	t.Helper()
	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          c.Keys.PairingPSK,
		cat:          categoryPairing,
		pair:         &pair,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	return peer, server, pairNow(t, c, peer, server)
}

func pairFromSentinel(t *testing.T, c *Client, ln net.Listener, pair noise.DHKey) (*wsPeer, *serverSide, []byte) {
	t.Helper()
	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
		pair:         &pair,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPairing},
		ActiveRoles: &[]string{},
		Pairing:     &activatePairing{Method: methodPairingPSK},
	}))
	kind, payload := nextJSON(t, peer, server)
	var a pairAbort
	_ = json.Unmarshal(payload, &a)
	if kind != typePairAbort || a.Reason != abortMethodNotSupported {
		t.Fatalf("got %s %q for pairing_psk on the Sentinel, want %s %q", kind, a.Reason,
			typePairAbort, abortMethodNotSupported)
	}
	server.rekey(t, peer, c.Keys.Identity.Public, c.Keys.PairingPSK, categoryPairing)
	return peer, server, pairNow(t, c, peer, server)
}

func pairNow(t *testing.T, c *Client, peer *wsPeer, server *serverSide) []byte {
	t.Helper()
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPairing},
		ActiveRoles: &[]string{},
		Pairing:     &activatePairing{Method: methodPairingPSK},
	}))
	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientPairInit {
		t.Fatalf("wanted %s, got %s", typeClientPairInit, kind)
	}
	var init pairInit
	if err := json.Unmarshal(payload, &init); err != nil {
		t.Fatalf("decoding %s: %v", kind, err)
	}
	if init.PairingIndex != 1 {
		t.Errorf("pairing_index = %d, want 1 for the first pairing activation since the"+
			" last handshake", init.PairingIndex)
	}
	kind, payload = nextJSON(t, peer, server)
	if kind != typeClientPairFinalize {
		t.Fatalf("wanted %s straight after %s, got %s", typeClientPairFinalize,
			typeClientPairInit, kind)
	}
	var fin pairFinalize
	if err := json.Unmarshal(payload, &fin); err != nil {
		t.Fatalf("decoding %s: %v", kind, err)
	}
	psk, err := base64.RawURLEncoding.DecodeString(fin.LongTermPSK)
	if err != nil || len(psk) != pskLen {
		t.Fatalf("long_term_psk %q is not 32 bytes of base64url: %v", fin.LongTermPSK, err)
	}
	if bytes.Equal(psk, c.Keys.PairingPSK) || bytes.Equal(psk, SentinelPSK()) {
		t.Fatal("the long-term psk is the pairing psk or the Sentinel")
	}
	return psk
}

func untilClosed(t *testing.T, peer *wsPeer) {
	t.Helper()
	for {
		if _, err := peer.r.Peek(1); err != nil {
			return
		}
		peer.read()
	}
}

func TestPairingPromotesTheConnectionToALongTermOne(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.PSKs.Records = testRecords(t)
	serveOn(t, c, ln)
	pair := serverKeypair(t)
	peer, server, psk := pairFromSentinel(t, c, ln, pair)

	peer.writeBinary(server.sealJSON(t, typeServerPairFinalize, struct{}{}))
	server.rekey(t, peer, c.Keys.Identity.Public, psk, categoryLongTerm)
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientState {
		t.Fatalf("wanted %s once the paired session holds the player, got %s",
			typeClientState, kind)
	}
	rec, ok := c.PSKs.Records.find(PSKID(psk))
	if !ok || !bytes.Equal(rec.server, pair.Public) {
		t.Fatalf("the record = %x, %v; want the long-term psk bound to the server", rec.server, ok)
	}
	if !c.protected()[PSKID(psk)] {
		t.Error("the promoted connection does not protect its record from eviction")
	}

	again := dialLocal(t, ln)
	second := driveServer(t, again, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          psk,
		cat:          categoryLongTerm,
		pair:         &pair,
	})
	again.writeBinary(second.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	if kind, _ := nextJSON(t, again, second); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}

	peer.writeBinary(server.sealJSON(t, typeServerUnpair, struct{}{}))
	for {
		kind, payload := nextJSON(t, peer, server)
		if kind == typeClientState {
			continue
		}
		var bye goodbye
		_ = json.Unmarshal(payload, &bye)
		if kind != typeClientGoodbye || bye.Reason != goodbyeUnpaired {
			t.Fatalf("got %s %q, want %s %q", kind, bye.Reason, typeClientGoodbye, goodbyeUnpaired)
		}
		break
	}
	untilClosed(t, peer)
	if _, ok := c.PSKs.Records.find(PSKID(psk)); ok {
		t.Error("the record survived server/unpair on the connection pairing promoted")
	}
}

func TestAServerAbortKeepsNothing(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.PSKs.Records = testRecords(t)
	serveOn(t, c, ln)
	peer, server, _ := pairOver(t, c, ln, serverKeypair(t))

	peer.writeBinary(server.sealJSON(t, typePairAbort, pairAbort{Reason: "user_cancelled"}))
	peer.writeBinary(server.sealJSON(t, typeServerPairFinalize, struct{}{}))
	handled(t, peer, server)
	if n := c.PSKs.Records.Len(); n != 0 {
		t.Errorf("%d records kept after the server aborted the attempt", n)
	}
}

func TestAnActivationInPlaceOfTheFinalizeKeepsNothing(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.PSKs.Records = testRecords(t)
	serveOn(t, c, ln)
	peer, server, _ := pairOver(t, c, ln, serverKeypair(t))

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{Activities: []string{}}))
	peer.writeBinary(server.sealJSON(t, typeServerPairFinalize, struct{}{}))
	handled(t, peer, server)
	if n := c.PSKs.Records.Len(); n != 0 {
		t.Errorf("%d records kept after the server cancelled the attempt", n)
	}
}

func TestAnUnansweredAttemptIsAbortedOnTimeout(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.PSKs.Records = testRecords(t)
	c.attemptAfter = 100 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := pairOver(t, c, ln, serverKeypair(t))

	kind, payload := nextJSON(t, peer, server)
	var a pairAbort
	_ = json.Unmarshal(payload, &a)
	if kind != typePairAbort || a.Reason != abortAttemptTimeout {
		t.Fatalf("got %s %q, want %s %q", kind, a.Reason, typePairAbort, abortAttemptTimeout)
	}
	peer.writeBinary(server.sealJSON(t, typeServerPairFinalize, struct{}{}))
	handled(t, peer, server)
	if n := c.PSKs.Records.Len(); n != 0 {
		t.Errorf("%d records kept for an attempt that had timed out", n)
	}
}

func TestTheDotSendsNothingBetweenARekeyAndItsActivation(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	server.rekey(t, peer, c.Keys.Identity.Public, SentinelPSK(), categorySentinel)
	c.SetDelay(50)
	time.Sleep(100 * time.Millisecond)
	if _, err := peer.conn.Write(frame(true, opPing, []byte("quiet"))); err != nil {
		t.Fatalf("writing a ping: %v", err)
	}
	for {
		op, body := peer.read()
		if op == opPong {
			break
		}
		if op == opBinary {
			_, msg := server.open(t, body)
			t.Fatalf("the dot sent %s before the server activated the new keys", msg)
		}
	}

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities: []string{activityPlayback},
	}))
	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("wanted the state held back during the rekey, got %s", kind)
	}
	var cs clientState
	if err := json.Unmarshal(payload, &cs); err != nil {
		t.Fatalf("decoding client/state: %v", err)
	}
	if cs.Player == nil || cs.Player.OutputDelayMS != 50 {
		t.Errorf("client/state = %+v, want the 50 ms delay set during the rekey", cs.Player)
	}
}

func TestARekeyOntoAPSKTheDotLacksClosesTheConnection(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	server.sendRekey(t, peer, c.Keys.Identity.Public, randomKey(t), categoryLongTerm)
	untilClosed(t, peer)
}

func TestUnpairOnAnUnpairedSessionIsIgnored(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	peer.writeBinary(server.sealJSON(t, typeServerUnpair, struct{}{}))
	handled(t, peer, server)
}
