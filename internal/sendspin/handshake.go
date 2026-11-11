package sendspin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	coreVersion = 1
	suiteName   = "25519_ChaChaPoly_SHA256"

	typeClientInit     = "client/init"
	typeServerInit     = "server/init"
	typeNoiseHandshake = "noise/handshake"
	typeServerError    = "server/error"

	maxCleartextFrame = 2048
	maxMessageFrames  = 4096

	emptyPayload = "{}"
)

var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

type category string

const (
	categoryLongTerm category = "lt"
	categoryPairing  category = "pr"
	categorySentinel category = "sn"
)

type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type clientInit struct {
	ClientID string `json:"client_id"`
	Version  int    `json:"version"`
	Suite    string `json:"suite"`
}

type serverInit struct {
	ServerID string `json:"server_id"`
	Version  int    `json:"version"`
}

type noiseFrame struct {
	Data string `json:"data"`
}

type handshakeIntro struct {
	PSKID    string   `json:"psk_id"`
	Category category `json:"psk_category"`
}

type serverErrorPayload struct {
	Reason string `json:"reason"`
}

var (
	errHandshake  = errors.New("sendspin handshake failed")
	errServerSaid = errors.New("server refused the init")
)

type PSKSet struct {
	Pairing []byte
	Records *Records
}

func (p PSKSet) selectPSK(id string, cat category, serverID string, fallback bool) ([]byte, category, error) {
	try := func(c category) ([]byte, error) {
		switch c {
		case categoryLongTerm:
			rec, ok := p.Records.find(id)
			if !ok {
				return nil, nil
			}
			if rec.serverID() != serverID {
				return nil, fmt.Errorf("%w: the long-term psk named belongs to another server",
					errHandshake)
			}
			return rec.psk, nil
		case categoryPairing:
			if len(p.Pairing) == pskLen && PSKID(p.Pairing) == id {
				return p.Pairing, nil
			}
		case categorySentinel:
			if sentinel := SentinelPSK(); PSKID(sentinel) == id {
				return sentinel, nil
			}
		}
		return nil, nil
	}

	var order []category
	switch cat {
	case "":
		order = []category{categoryLongTerm, categoryPairing, categorySentinel}
	case categoryLongTerm, categoryPairing, categorySentinel:
		order = []category{cat}
	default:
		return nil, "", fmt.Errorf("%w: unknown psk_category %q", errHandshake, untrustedlog.Cut(string(cat)))
	}
	for _, c := range order {
		psk, err := try(c)
		if err != nil {
			return nil, "", err
		}
		if psk != nil {
			return psk, c, nil
		}
	}
	if !fallback {
		return nil, "", fmt.Errorf("%w: no psk matches the one the server named", errHandshake)
	}
	return SentinelPSK(), categorySentinel, nil
}

type keyed struct {
	send     *noise.CipherState
	recv     *noise.CipherState
	matched  category
	longTerm string
	hash     []byte
}

func respond(keys Keys, peer, prologue, first []byte, psks PSKSet, serverID string,
	fallback bool) ([]byte, keyed, error) {

	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeKK,
		Initiator:             false,
		PresharedKeyPlacement: 2,
		StaticKeypair: noise.DHKey{
			Private: keys.Identity.Private,
			Public:  keys.Identity.Public,
		},
		PeerStatic: peer,
		Prologue:   prologue,
	})
	if err != nil {
		return nil, keyed{}, fmt.Errorf("%w: %w", errHandshake, err)
	}
	intro, _, _, err := hs.ReadMessage(nil, first)
	if err != nil {
		return nil, keyed{}, fmt.Errorf("%w: noise message 1: %w", errHandshake, err)
	}
	var hi handshakeIntro
	if err := json.Unmarshal(intro, &hi); err != nil {
		return nil, keyed{}, fmt.Errorf("%w: noise message 1 payload: %w", errHandshake, err)
	}
	psk, matched, err := psks.selectPSK(hi.PSKID, hi.Category, serverID, fallback)
	if err != nil {
		return nil, keyed{}, err
	}
	if err := hs.SetPresharedKey(psk); err != nil {
		return nil, keyed{}, fmt.Errorf("%w: %w", errHandshake, err)
	}
	second, cs1, cs2, err := hs.WriteMessage(nil, []byte(emptyPayload))
	if err != nil {
		return nil, keyed{}, fmt.Errorf("%w: noise message 2: %w", errHandshake, err)
	}
	if cs1 == nil || cs2 == nil {
		return nil, keyed{}, fmt.Errorf("%w: handshake did not complete on message 2", errHandshake)
	}
	k := keyed{send: cs2, recv: cs1, matched: matched, hash: hs.ChannelBinding()}
	if matched == categoryLongTerm {
		k.longTerm = PSKID(psk)
	}
	return second, k, nil
}

func noiseEnvelope(data []byte) ([]byte, error) {
	return marshalEnvelope(typeNoiseHandshake, noiseFrame{
		Data: base64.RawURLEncoding.EncodeToString(data),
	})
}

type Session struct {
	ws   *Conn
	send *noise.CipherState
	recv *noise.CipherState

	matched  category
	serverID string
	peer     []byte
	hash     []byte
	longTerm atomic.Value

	quiet bool

	pairing      sync.Mutex
	pairIndex    int
	pending      []byte
	pendingTimer *time.Timer
	attemptWait  time.Duration

	writing sync.Mutex
	report  chan struct{}

	displaced atomic.Bool
	streams   streamGate

	inFragment   bool
	fragment     []byte
	fragmentType byte

	offered    map[string]pairMethod
	unpaired   bool
	roles      []string
	activities []string

	streaming bool
	flac      bool
	rate      int

	clock *clock
}

func (s *Session) Matched() category { return s.matched }

func (s *Session) ServerID() string { return s.serverID }

func Handshake(ws *Conn, keys Keys, psks PSKSet) (*Session, error) {
	ws.setReadLimit(maxCleartextFrame)

	initBytes, err := marshalEnvelope(typeClientInit, clientInit{
		ClientID: keys.Identity.ClientID(),
		Version:  coreVersion,
		Suite:    suiteName,
	})
	if err != nil {
		return nil, err
	}
	if err := ws.WriteText(initBytes); err != nil {
		return nil, err
	}

	serverInitBytes, payload, err := readCleartext(ws, typeServerInit)
	if err != nil {
		return nil, err
	}
	var si serverInit
	if err := json.Unmarshal(payload, &si); err != nil {
		return nil, fmt.Errorf("%w: server/init: %w", errHandshake, err)
	}
	if si.Version != coreVersion {
		return nil, fmt.Errorf("%w: server speaks core version %d, not %d",
			errHandshake, si.Version, coreVersion)
	}
	peer, err := DecodeID(si.ServerID)
	if err != nil {
		return nil, fmt.Errorf("%w: server_id: %w", errHandshake, err)
	}
	serverID := EncodeID(peer)

	_, first, err := readNoiseMessage(ws)
	if err != nil {
		return nil, err
	}
	prologue := append(append([]byte{}, initBytes...), serverInitBytes...)
	second, k, err := respond(keys, peer, prologue, first, psks, serverID, true)
	if err != nil {
		return nil, err
	}
	frame, err := noiseEnvelope(second)
	if err != nil {
		return nil, err
	}
	if err := ws.WriteText(frame); err != nil {
		return nil, err
	}

	ws.setReadLimit(maxMessage)
	s := &Session{
		ws:       ws,
		send:     k.send,
		recv:     k.recv,
		matched:  k.matched,
		serverID: serverID,
		peer:     peer,
		hash:     k.hash,
		clock:    newClock(),
		report:   make(chan struct{}, 1),
	}
	s.longTerm.Store(k.longTerm)
	return s, nil
}

func marshalEnvelope(kind string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", kind, err)
	}
	out, err := json.Marshal(envelope{Type: kind, Payload: raw})
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", kind, err)
	}
	return out, nil
}

func readCleartext(ws *Conn, want string) (raw []byte, payload json.RawMessage, err error) {
	isBinary, raw, err := ws.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("waiting for %s: %w", want, err)
	}
	if isBinary {
		return nil, nil, fmt.Errorf("%w: binary frame before transport mode", errHandshake)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errHandshake, err)
	}
	if env.Type == typeServerError && want != typeServerError {
		var se serverErrorPayload
		_ = json.Unmarshal(env.Payload, &se)
		return nil, nil, fmt.Errorf("%w: %q", errServerSaid, untrustedlog.Cut(se.Reason))
	}
	if env.Type != want {
		return nil, nil, fmt.Errorf("%w: wanted %s, got %q", errHandshake, want, untrustedlog.Cut(env.Type))
	}
	return raw, env.Payload, nil
}

func readNoiseMessage(ws *Conn) (raw []byte, data []byte, err error) {
	raw, payload, err := readCleartext(ws, typeNoiseHandshake)
	if err != nil {
		return nil, nil, err
	}
	var nf noiseFrame
	if err := json.Unmarshal(payload, &nf); err != nil {
		return nil, nil, fmt.Errorf("%w: noise/handshake: %w", errHandshake, err)
	}
	data, err = base64.RawURLEncoding.DecodeString(nf.Data)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: noise/handshake data: %w", errHandshake, err)
	}
	return raw, data, nil
}
