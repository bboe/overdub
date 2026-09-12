package sendspin

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
)

type wsPeer struct {
	conn net.Conn
	r    *bufio.Reader
	t    *testing.T
}

func newWSPeer(t *testing.T, c net.Conn) *wsPeer {
	return &wsPeer{conn: c, r: bufio.NewReader(c), t: t}
}

func (p *wsPeer) upgrade(path string) {
	p.t.Helper()
	if _, err := io.WriteString(p.conn,
		upgradeRequest(path, rfcKey, "13", "websocket", "Upgrade")); err != nil {
		p.t.Fatalf("writing the upgrade request: %v", err)
	}
	status, err := readLine(p.r)
	if err != nil {
		p.t.Fatalf("reading the upgrade response: %v", err)
	}
	if !strings.Contains(status, "101") {
		p.t.Fatalf("upgrade response = %q", status)
	}
	for {
		line, err := readLine(p.r)
		if err != nil {
			p.t.Fatalf("reading the upgrade headers: %v", err)
		}
		if line == "" {
			return
		}
	}
}

func (p *wsPeer) writeText(b []byte) {
	p.t.Helper()
	if _, err := p.conn.Write(frame(true, opText, b)); err != nil {
		p.t.Fatalf("writing a text frame: %v", err)
	}
}

func (p *wsPeer) writeBinary(b []byte) {
	p.t.Helper()
	if _, err := p.conn.Write(frame(true, opBinary, b)); err != nil {
		p.t.Fatalf("writing a binary frame: %v", err)
	}
}

// pushBinary is writeBinary for a peer the client is expected to stop reading
// partway: the write fails once the connection goes, and that is the point.
func (p *wsPeer) pushBinary(b []byte) error {
	_, err := p.conn.Write(frame(true, opBinary, b))
	return err
}

func (p *wsPeer) read() (opcode, []byte) {
	p.t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(p.r, h[:]); err != nil {
		p.t.Fatalf("reading a frame header: %v", err)
	}
	if h[1]&0x80 != 0 {
		p.t.Fatal("a server frame arrived masked")
	}
	length := uint64(h[1] & 0x7f)
	switch length {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(p.r, e[:]); err != nil {
			p.t.Fatalf("reading a 16-bit length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(p.r, e[:]); err != nil {
			p.t.Fatalf("reading a 64-bit length: %v", err)
		}
		length = binary.BigEndian.Uint64(e[:])
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(p.r, body); err != nil {
		p.t.Fatalf("reading a frame payload: %v", err)
	}
	return opcode(h[0] & 0x0f), body
}

func (p *wsPeer) readEnvelope(want string) json.RawMessage {
	p.t.Helper()
	op, body := p.read()
	if op != opText {
		p.t.Fatalf("wanted a text frame, got opcode %#x", op)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		p.t.Fatalf("decoding %s: %v", want, err)
	}
	if env.Type != want {
		p.t.Fatalf("wanted %s, got %s", want, env.Type)
	}
	return env.Payload
}

func (p *wsPeer) readRaw(want string) []byte {
	p.t.Helper()
	op, body := p.read()
	if op != opText {
		p.t.Fatalf("wanted a text frame, got opcode %#x", op)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		p.t.Fatalf("decoding %s: %v", want, err)
	}
	if env.Type != want {
		p.t.Fatalf("wanted %s, got %s", want, env.Type)
	}
	return body
}

type serverSide struct {
	send *noise.CipherState
	recv *noise.CipherState
	id   string
}

func (v *serverSide) sealJSON(t *testing.T, kind string, payload any) []byte {
	t.Helper()
	body, err := marshalEnvelope(kind, payload)
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	sealed, err := v.send.Encrypt(nil, nil, append([]byte{msgJSON}, body...))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return sealed
}

func (v *serverSide) seal(t *testing.T, plain []byte) []byte {
	t.Helper()
	sealed, err := v.send.Encrypt(nil, nil, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return sealed
}

func (v *serverSide) open(t *testing.T, sealed []byte) (byte, []byte) {
	t.Helper()
	plain, err := v.recv.Decrypt(nil, nil, sealed)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if len(plain) == 0 {
		t.Fatal("decrypted an empty plaintext")
	}
	return plain[0], plain[1:]
}

type serverPlan struct {
	clientPublic []byte
	psk          []byte
	advertisedID string
	cat          category

	// pair is the server's static keypair. A test supplies one when it has to know
	// the server_id before it connects -- the client snapshots its pairing records
	// when the connection is accepted, which is before server/init arrives.
	pair *noise.DHKey

	// initJSON replaces the server/init frame with these exact bytes. The prologue
	// is the bytes on the wire, so a server whose encoder spaces or orders its JSON
	// differently from ours only agrees with us if we kept what it sent.
	initJSON func(serverID string) []byte
}

// driveServer plays the Sendspin server: it answers client/init, runs the Noise
// handshake as initiator, and hands back the two cipher states.
func driveServer(t *testing.T, p *wsPeer, plan serverPlan) *serverSide {
	t.Helper()
	p.upgrade("/sendspin")
	clientInitBytes := p.readRaw(typeClientInit)

	pair := noise.DHKey{}
	if plan.pair != nil {
		pair = *plan.pair
	} else {
		var err error
		pair, err = noise.DH25519.GenerateKeypair(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKeypair: %v", err)
		}
	}
	serverID := EncodeID(pair.Public)
	serverInitBytes, err := marshalEnvelope(typeServerInit, serverInit{
		ServerID: serverID,
		Version:  coreVersion,
	})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	if plan.initJSON != nil {
		serverInitBytes = plan.initJSON(serverID)
	}
	p.writeText(serverInitBytes)

	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeKK,
		Initiator:             true,
		PresharedKeyPlacement: 2,
		StaticKeypair:         pair,
		PeerStatic:            plan.clientPublic,
		Prologue:              append(append([]byte{}, clientInitBytes...), serverInitBytes...),
	})
	if err != nil {
		t.Fatalf("NewHandshakeState: %v", err)
	}
	if err := hs.SetPresharedKey(plan.psk); err != nil {
		t.Fatalf("SetPresharedKey: %v", err)
	}
	advertised := plan.advertisedID
	if advertised == "" {
		advertised = PSKID(plan.psk)
	}
	intro, err := json.Marshal(handshakeIntro{PSKID: advertised, Category: plan.cat})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg1, _, _, err := hs.WriteMessage(nil, intro)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	frame1, err := marshalEnvelope(typeNoiseHandshake, noiseFrame{
		Data: base64.RawURLEncoding.EncodeToString(msg1),
	})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	p.writeText(frame1)

	payload := p.readEnvelope(typeNoiseHandshake)
	var nf noiseFrame
	if err := json.Unmarshal(payload, &nf); err != nil {
		t.Fatalf("decoding noise message 2: %v", err)
	}
	msg2, err := base64.RawURLEncoding.DecodeString(nf.Data)
	if err != nil {
		t.Fatalf("decoding noise message 2 data: %v", err)
	}
	inner, cs1, cs2, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(inner) != emptyPayload {
		t.Errorf("noise message 2 payload = %q, want %q", inner, emptyPayload)
	}
	if cs1 == nil || cs2 == nil {
		t.Fatal("server handshake did not complete on message 2")
	}
	return &serverSide{send: cs1, recv: cs2, id: serverID}
}

func pipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	deadline := time.Now().Add(10 * time.Second)
	if err := c1.SetDeadline(deadline); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if err := c2.SetDeadline(deadline); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	t.Cleanup(func() { c1.Close(); c2.Close() })
	return c1, c2
}

func testKeys(t *testing.T) Keys {
	t.Helper()
	keys, _, err := LoadOrCreateKeys(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	return keys
}

type handshakeResult struct {
	s   *Session
	err error
}

func startClient(c net.Conn, keys Keys, psks PSKSet) <-chan handshakeResult {
	done := make(chan handshakeResult, 1)
	go func() {
		ws, err := Accept(c, "/sendspin")
		if err != nil {
			done <- handshakeResult{nil, err}
			return
		}
		s, err := Handshake(ws, keys, psks)
		done <- handshakeResult{s, err}
	}()
	return done
}

func handshakePair(t *testing.T, keys Keys, psks PSKSet, plan serverPlan) (*Session, *serverSide, *wsPeer) {
	t.Helper()
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, psks)
	plan.clientPublic = keys.Identity.Public
	peer := newWSPeer(t, c2)
	server := driveServer(t, peer, plan)
	got := <-done
	if got.err != nil {
		t.Fatalf("Handshake: %v", got.err)
	}
	return got.s, server, peer
}

func pairedSession(t *testing.T) (*Session, *serverSide, *wsPeer) {
	t.Helper()
	keys := testKeys(t)
	return handshakePair(t, keys,
		PSKSet{Pairing: keys.PairingPSK},
		serverPlan{psk: keys.PairingPSK, cat: categoryPairing})
}

func TestHandshakeCompletesAgainstAServer(t *testing.T) {
	session, server, _ := pairedSession(t)
	if session.Matched() != categoryPairing {
		t.Errorf("matched category = %q, want %q", session.Matched(), categoryPairing)
	}
	if session.ServerID() != server.id {
		t.Errorf("server id = %q, want %q", session.ServerID(), server.id)
	}
	if len(session.binding) != 32 {
		t.Errorf("channel binding is %d bytes, want 32", len(session.binding))
	}
}

func TestTransportCarriesAMessageToTheServer(t *testing.T) {
	session, server, peer := pairedSession(t)
	write := make(chan error, 1)
	go func() {
		write <- session.WriteJSON("client/hello", map[string]any{"name": "kitchen"})
	}()
	op, sealed := peer.read()
	if op != opBinary {
		t.Fatalf("transport frame arrived as opcode %#x, want binary", op)
	}
	if err := <-write; err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	kind, body := server.open(t, sealed)
	if kind != msgJSON {
		t.Errorf("message type = %#x, want %#x", kind, msgJSON)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decoding what the server received: %v", err)
	}
	if env.Type != "client/hello" {
		t.Errorf("type = %q", env.Type)
	}
	if !bytes.Contains(env.Payload, []byte("kitchen")) {
		t.Errorf("payload = %s", env.Payload)
	}
}

func TestTransportCarriesAMessageFromTheServer(t *testing.T) {
	session, server, peer := pairedSession(t)
	go peer.writeBinary(server.sealJSON(t, "server/hello", map[string]any{"name": "music assistant"}))
	kind, payload, err := session.ReadEnvelope()
	if err != nil {
		t.Fatalf("ReadEnvelope: %v", err)
	}
	if kind != "server/hello" {
		t.Errorf("type = %q, want server/hello", kind)
	}
	if !bytes.Contains(payload, []byte("music assistant")) {
		t.Errorf("payload = %s", payload)
	}
}

func TestTransportRefusesACleartextFrameAfterHandshake(t *testing.T) {
	// A text frame carrying a validly sealed payload: the bytes would decrypt if the
	// reader looked, so only the text-versus-binary check can refuse this. Asserting
	// errTransport alone passes either way, because a failed decrypt says the same.
	session, server, peer := pairedSession(t)
	sealed := server.sealJSON(t, "server/anything", struct{ N int }{1})
	go peer.writeText(sealed)
	_, _, err := session.Read()
	if !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want %v", err, errTransport)
	}
	if !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("err = %v; refused for the wrong reason, so the frame type is unchecked", err)
	}
}

func TestTransportRefusesAForgedFrame(t *testing.T) {
	session, _, peer := pairedSession(t)
	go peer.writeBinary(bytes.Repeat([]byte{0xff}, 64))
	if _, _, err := session.Read(); !errors.Is(err, errTransport) {
		t.Errorf("err = %v, want %v", err, errTransport)
	}
}

func TestHandshakeFallsBackToTheSentinelOnALookupMiss(t *testing.T) {
	keys := testKeys(t)
	unknown := make([]byte, pskLen)
	for i := range unknown {
		unknown[i] = 0x5a
	}
	session, _, _ := handshakePair(t, keys,
		PSKSet{Pairing: keys.PairingPSK},
		serverPlan{
			psk:          SentinelPSK(),
			advertisedID: PSKID(unknown),
			cat:          categoryLongTerm,
		})
	if session.Matched() != categorySentinel {
		t.Errorf("matched category = %q, want %q", session.Matched(), categorySentinel)
	}
}

func TestHandshakeReportsAServerError(t *testing.T) {
	keys := testKeys(t)
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
	peer := newWSPeer(t, c2)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)
	body, err := marshalEnvelope(typeServerError, serverErrorPayload{Reason: "unsupported_suite"})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	peer.writeText(body)
	got := <-done
	if !errors.Is(got.err, errServerSaid) {
		t.Fatalf("err = %v, want %v", got.err, errServerSaid)
	}
	if !strings.Contains(got.err.Error(), "unsupported_suite") {
		t.Errorf("error does not carry the reason: %v", got.err)
	}
}

func TestHandshakeRefusesAnotherCoreVersion(t *testing.T) {
	keys := testKeys(t)
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
	peer := newWSPeer(t, c2)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)
	body, err := marshalEnvelope(typeServerInit, serverInit{ServerID: EncodeID(keys.Identity.Public), Version: 2})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	peer.writeText(body)
	got := <-done
	if !errors.Is(got.err, errHandshake) {
		t.Fatalf("err = %v, want %v", got.err, errHandshake)
	}
	if !strings.Contains(got.err.Error(), "core version 2") {
		t.Errorf("error does not name the version: %v", got.err)
	}
}

func TestClientInitAnnouncesTheSuiteItHandshakesWith(t *testing.T) {
	if got := string(noiseSuite.Name()); got != suiteName {
		t.Errorf("noise suite is %q but client/init advertises %q", got, suiteName)
	}
}

func TestClientInitCarriesTheIdentity(t *testing.T) {
	keys := testKeys(t)
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
	peer := newWSPeer(t, c2)
	peer.upgrade("/sendspin")
	payload := peer.readEnvelope(typeClientInit)
	var ci clientInit
	if err := json.Unmarshal(payload, &ci); err != nil {
		t.Fatalf("decoding client/init: %v", err)
	}
	if ci.ClientID != keys.Identity.ClientID() {
		t.Errorf("client_id = %q, want %q", ci.ClientID, keys.Identity.ClientID())
	}
	if ci.Version != coreVersion {
		t.Errorf("version = %d, want %d", ci.Version, coreVersion)
	}
	if ci.Suite != suiteName {
		t.Errorf("suite = %q, want %q", ci.Suite, suiteName)
	}
	c2.Close()
	<-done
}

func TestSelectPSKMissesAKeyHeldUnderAnotherCategory(t *testing.T) {
	psk := bytes.Repeat([]byte{0x22}, pskLen)
	psks := PSKSet{Pairing: psk}
	got, cat, err := psks.selectPSK(PSKID(psk), categoryLongTerm)
	if err != nil {
		t.Fatalf("selectPSK: %v", err)
	}
	if cat != categorySentinel || !bytes.Equal(got, SentinelPSK()) {
		t.Errorf("category = %q, want the sentinel fallback", cat)
	}
}

func TestSelectPSKMatchesTheSentinel(t *testing.T) {
	psks := PSKSet{}
	got, cat, err := psks.selectPSK(PSKID(SentinelPSK()), categorySentinel)
	if err != nil {
		t.Fatalf("selectPSK: %v", err)
	}
	if cat != categorySentinel || !bytes.Equal(got, SentinelPSK()) {
		t.Error("did not match the sentinel psk")
	}
}

func TestAHandshakeWithNoCategoryMatchesEveryCandidate(t *testing.T) {
	keys := testKeys(t)
	set := PSKSet{Pairing: keys.PairingPSK}

	for _, c := range []struct {
		name string
		psk  []byte
		want category
	}{
		{"the sentinel", SentinelPSK(), categorySentinel},
		{"a pairing psk", keys.PairingPSK, categoryPairing},
	} {
		t.Run(c.name, func(t *testing.T) {
			psk, matched, err := set.selectPSK(PSKID(c.psk), "")
			if err != nil {
				t.Fatalf("an absent psk_category was refused: %v; aiosendspin 9.1.1 sends none,"+
					" so this is every real handshake", err)
			}
			if matched != c.want {
				t.Errorf("matched %q, want %q", matched, c.want)
			}
			if string(psk) != string(c.psk) {
				t.Error("selected a different key than the psk_id named")
			}
		})
	}
}

func TestThePrologueIsTheBytesTheServerSent(t *testing.T) {
	// aiosendspin's encoder spaces its JSON and orders the keys its own way. The
	// prologue is a hash input, so re-serialising the parsed struct agrees with
	// our own encoder and with no one else's.
	keys := testKeys(t)
	session, _, _ := handshakePair(t, keys, PSKSet{Pairing: keys.PairingPSK}, serverPlan{
		psk: keys.PairingPSK,
		cat: categoryPairing,
		initJSON: func(serverID string) []byte {
			return []byte(fmt.Sprintf(
				`{"payload": {"server_id": %q, "version": %d}, "type": "server/init"}`,
				serverID, coreVersion))
		},
	})
	if session == nil {
		t.Fatal("no session")
	}
}

func TestAServerIDIsKeptInItsCanonicalForm(t *testing.T) {
	// Go's base64 decoder ignores the unused trailing bits, so one 32-byte key has
	// several spellings. The id is a pairing record's map key and the comparand the
	// misbinding check refuses on, so the spelling a peer chose is not the identity.
	keys := testKeys(t)
	var sent string
	session, server, _ := handshakePair(t, keys, PSKSet{Pairing: keys.PairingPSK}, serverPlan{
		psk: keys.PairingPSK,
		cat: categoryPairing,
		initJSON: func(serverID string) []byte {
			raw, err := DecodeID(serverID)
			if err != nil {
				t.Fatalf("DecodeID: %v", err)
			}
			alt := []byte(serverID)
			alt[len(alt)-1] = flipTrailingBits(t, raw)
			sent = string(alt)
			return []byte(fmt.Sprintf(`{"type":%q,"payload":{"server_id":%q,"version":%d}}`,
				typeServerInit, sent, coreVersion))
		},
	})
	if sent == server.id {
		t.Fatal("the test sent the canonical spelling, so it proves nothing")
	}
	if session.ServerID() != server.id {
		t.Errorf("server id = %q, want the canonical %q (the peer spelled it %q)",
			session.ServerID(), server.id, sent)
	}
}

func flipTrailingBits(t *testing.T, key []byte) byte {
	t.Helper()
	canonical := EncodeID(key)
	last := canonical[len(canonical)-1]
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, last)
	if i < 0 {
		t.Fatalf("%q is not base64url", last)
	}
	for _, cand := range alphabet {
		if byte(cand) == last {
			continue
		}
		alt := canonical[:len(canonical)-1] + string(cand)
		raw, err := DecodeID(alt)
		if err == nil && bytes.Equal(raw, key) {
			return byte(cand)
		}
	}
	t.Fatal("no other spelling of this key exists")
	return 0
}

func TestConcurrentWritesDoNotShareANonce(t *testing.T) {
	session, server, peer := pairedSession(t)

	// Enough contention that the unlocked version fails every run rather than most:
	// a nonce collision needs two writers inside Encrypt at once.
	const writers = 8
	const each = 25
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := session.WriteJSON("test/message", struct{ N int }{j}); err != nil {
					t.Errorf("WriteJSON: %v", err)
					return
				}
			}
		}(i)
	}

	read := make(chan error, 1)
	go func() {
		for i := 0; i < writers*each; i++ {
			_, sealed := peer.read()
			if _, err := server.recv.Decrypt(nil, nil, sealed); err != nil {
				read <- fmt.Errorf("message %d did not authenticate, so two writers shared"+
					" a nonce: %w", i, err)
				return
			}
		}
		read <- nil
	}()
	wg.Wait()
	if err := <-read; err != nil {
		t.Error(err)
	}
}

func TestAMessageSplitAcrossTooManySendspinFramesIsRefused(t *testing.T) {
	session, server, peer := pairedSession(t)

	go func() {
		if err := peer.pushBinary(server.seal(t, []byte{msgFragment, fragFirst, msgJSON})); err != nil {
			return
		}
		for i := 0; i < maxMessageFrames+10; i++ {
			if err := peer.pushBinary(server.seal(t, []byte{msgFragment, 0})); err != nil {
				return
			}
		}
	}()
	if _, _, err := session.Read(); !errors.Is(err, errTransport) {
		t.Errorf("err = %v, want %v; an endless chain of empty fragments never reaches"+
			" the byte ceiling", err, errTransport)
	}
}

func TestHandshakeIgnoresAPairingPSKOfTheWrongLength(t *testing.T) {
	keys := testKeys(t)
	short := keys.PairingPSK[:pskLen-1]
	// The server names the short key's psk_id. Nothing must match it, so the
	// handshake falls back to the sentinel rather than using a key of the wrong size.
	session, _, _ := handshakePair(t, keys,
		PSKSet{Pairing: short},
		serverPlan{psk: SentinelPSK(), cat: categoryPairing, advertisedID: PSKID(short)})
	if session.Matched() != categorySentinel {
		t.Errorf("matched %q, want %q", session.Matched(), categorySentinel)
	}
}

func TestAHandshakeErrorCannotCarryAForgedLogLine(t *testing.T) {
	// These errors reach the log through one %v at the call site, and a server/error
	// reason arrives in cleartext before any key is involved.
	nasty := strings.Repeat("a", 1800) + "\nsendspin: forged by the peer"
	for _, c := range []struct {
		name string
		send func(*wsPeer)
	}{
		{"a server/error reason", func(p *wsPeer) {
			body, err := marshalEnvelope(typeServerError, serverErrorPayload{Reason: nasty})
			if err != nil {
				t.Fatalf("marshalEnvelope: %v", err)
			}
			p.writeText(body)
		}},
		{"an envelope type", func(p *wsPeer) {
			body, err := marshalEnvelope(nasty, struct{}{})
			if err != nil {
				t.Fatalf("marshalEnvelope: %v", err)
			}
			p.writeText(body)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			keys := testKeys(t)
			c1, c2 := pipePair(t)
			done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
			peer := newWSPeer(t, c2)
			peer.upgrade("/sendspin")
			peer.readRaw(typeClientInit)
			c.send(peer)

			got := <-done
			if got.err == nil {
				t.Fatal("the handshake accepted it")
			}
			text := got.err.Error()
			if strings.Contains(text, "\n") {
				t.Errorf("the error carries a raw newline, so one log line becomes two:\n%s", text)
			}
			if len(text) > 256 {
				t.Errorf("the error is %d bytes; a peer sets how much of the log it fills", len(text))
			}
		})
	}
}

func TestClientInitIsExactlyThisOnTheWire(t *testing.T) {
	// A literal, because every other test here reads the fields back through the
	// same structs that wrote them: rename a JSON tag and they all still agree.
	got, err := marshalEnvelope(typeClientInit, clientInit{
		ClientID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Version:  coreVersion,
		Suite:    suiteName,
	})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	const want = `{"type":"client/init","payload":{` +
		`"client_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",` +
		`"version":1,"suite":"25519_ChaChaPoly_SHA256"}}`
	if string(got) != want {
		t.Errorf("client/init is\n%s\nwant\n%s", got, want)
	}
}

func TestTheProtocolStringsAreFixed(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"client/init", typeClientInit, "client/init"},
		{"server/init", typeServerInit, "server/init"},
		{"noise/handshake", typeNoiseHandshake, "noise/handshake"},
		{"server/error", typeServerError, "server/error"},
		{"long-term category", string(categoryLongTerm), "lt"},
		{"pairing category", string(categoryPairing), "pr"},
		{"sentinel category", string(categorySentinel), "sn"},
		{"psk id label", pskIDLabel, "sendspin-psk-id-v1"},
		{"empty payload", emptyPayload, "{}"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if coreVersion != 1 {
		t.Errorf("core version = %d, want 1", coreVersion)
	}
}

func TestReadCleartextRefusesTheWrongEnvelopeType(t *testing.T) {
	keys := testKeys(t)
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
	peer := newWSPeer(t, c2)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)

	// Where server/init is expected, a well-formed envelope of another type is not a
	// substitute for it.
	body, err := marshalEnvelope("server/something-else", struct{ N int }{1})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	peer.writeText(body)
	got := <-done
	if got.err == nil {
		t.Fatal("the handshake accepted server/hello where it asked for server/init")
	}
	if !strings.Contains(got.err.Error(), typeServerInit) {
		t.Errorf("err = %v, want it to name %s", got.err, typeServerInit)
	}
}

func TestTheReadLimitsAreFixedAndNarrowFirst(t *testing.T) {
	// An unauthenticated peer gets the small limit; it widens only once the handshake
	// is done. Every other test sets a limit of its own, so neither the values nor
	// the order they are applied in is otherwise pinned.
	if maxCleartextFrame != 2048 {
		t.Errorf("cleartext limit = %d, want 2048", maxCleartextFrame)
	}
	if maxMessage != 65535 {
		t.Errorf("message limit = %d, want 65535", maxMessage)
	}
	if maxCleartextFrame >= maxMessage {
		t.Error("the handshake limit is not narrower than the transport one")
	}

	keys := testKeys(t)
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
	peer := newWSPeer(t, c2)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)

	// One cleartext frame past the handshake limit but well inside the transport one.
	big := make([]byte, maxCleartextFrame+1)
	for i := range big {
		big[i] = 'x'
	}
	peer.writeText(big)
	if got := <-done; got.err == nil {
		t.Fatal("a frame past the cleartext limit was accepted before the handshake")
	}
}

func TestTheInboundShapesAreFixedAgainstLiterals(t *testing.T) {
	// These three are read and never written, so the test peer marshals them with the
	// same structs the client reads: rename a tag and both sides move together. Only
	// the reference server would notice, and it runs in a job of its own.
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"server/init", serverInit{ServerID: "sid", Version: 1},
			`{"server_id":"sid","version":1}`},
		{"noise/handshake", noiseFrame{Data: "ZGF0YQ"},
			`{"data":"ZGF0YQ"}`},
		{"noise message 1 payload", handshakeIntro{PSKID: "pid", Category: categoryLongTerm},
			`{"psk_id":"pid","psk_category":"lt"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(c.v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("%s is\n%s\nwant\n%s", c.name, got, c.want)
			}
		})
	}
}

func TestABinaryFrameBeforeTransportModeIsRefused(t *testing.T) {
	// Everything before transport mode is text. A binary frame there is a peer that
	// has skipped the handshake, and the check that says so had no test.
	keys := testKeys(t)
	c1, c2 := pipePair(t)
	done := startClient(c1, keys, PSKSet{Pairing: keys.PairingPSK})
	peer := newWSPeer(t, c2)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)
	peer.writeBinary([]byte(`{"type":"server/init","payload":{"server_id":"x","version":1}}`))

	got := <-done
	if got.err == nil {
		t.Fatal("a binary frame was accepted before transport mode")
	}
	if !strings.Contains(got.err.Error(), "binary") {
		t.Errorf("err = %v; refused for the wrong reason", got.err)
	}
}
