package sendspin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	typeClientPairInit     = "client/pair-init"
	typeClientPairFinalize = "client/pair-finalize"
	typeServerPairFinalize = "server/pair-finalize"
	typeServerUnpair       = "server/unpair"

	abortAttemptTimeout = "attempt_timeout"

	attemptWait = 2 * time.Minute
)

type pairInit struct {
	PairingIndex int `json:"pairing_index"`
}

type pairFinalize struct {
	LongTermPSK string `json:"long_term_psk"`
}

var errUnpaired = errors.New("the server dropped its pairing record, so its connection goes")

func (s *Session) LongTerm() string {
	id, _ := s.longTerm.Load().(string)
	return id
}

func (s *Session) Rehandshake(payload json.RawMessage, keys Keys, psks PSKSet) error {
	s.pairing.Lock()
	s.dropAttempt()
	s.pairIndex = 0
	s.pairing.Unlock()
	s.writing.Lock()
	defer s.writing.Unlock()
	s.quiet = true

	var nf noiseFrame
	if err := json.Unmarshal(payload, &nf); err != nil {
		return fmt.Errorf("%w: re-handshake: %w", errHandshake, err)
	}
	first, err := base64.RawURLEncoding.DecodeString(nf.Data)
	if err != nil {
		return fmt.Errorf("%w: re-handshake data: %w", errHandshake, err)
	}
	second, k, err := respond(keys, s.peer, s.hash, first, psks, s.serverID, false)
	if err != nil {
		return err
	}
	frame, err := noiseEnvelope(second)
	if err != nil {
		return err
	}
	if err := s.seal(append([]byte{msgJSON}, frame...)); err != nil {
		return err
	}
	s.send, s.recv, s.matched, s.hash = k.send, k.recv, k.matched, k.hash
	s.longTerm.Store(k.longTerm)
	return nil
}

func (s *Session) endQuiet() {
	s.writing.Lock()
	defer s.writing.Unlock()
	s.quiet = false
}

func (s *Session) endAttempt() {
	s.pairing.Lock()
	defer s.pairing.Unlock()
	s.dropAttempt()
}

func (s *Session) dropAttempt() {
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
		s.pendingTimer = nil
	}
	s.pending = nil
}

func (s *Session) countPairing() {
	s.pairing.Lock()
	defer s.pairing.Unlock()
	s.dropAttempt()
	s.pairIndex++
}

func (s *Session) startPairing(wait time.Duration) error {
	psk, err := NewPSK()
	if err != nil {
		return err
	}
	if bytes.Equal(psk, SentinelPSK()) || PSKID(psk) == s.LongTerm() {
		return fmt.Errorf("drew a pre-shared key already in use")
	}
	s.pairing.Lock()
	defer s.pairing.Unlock()
	if err := s.WriteJSON(typeClientPairInit, pairInit{PairingIndex: s.pairIndex}); err != nil {
		return err
	}
	if err := s.WriteJSON(typeClientPairFinalize, pairFinalize{
		LongTermPSK: base64.RawURLEncoding.EncodeToString(psk),
	}); err != nil {
		return err
	}
	s.pending = psk
	var timer *time.Timer
	timer = time.AfterFunc(wait, func() {
		s.pairing.Lock()
		defer s.pairing.Unlock()
		if s.pendingTimer != timer {
			return
		}
		s.dropAttempt()
		_ = s.WriteJSON(typePairAbort, pairAbort{Reason: abortAttemptTimeout})
	})
	s.pendingTimer = timer
	return nil
}

func (s *Session) finalized() []byte {
	s.pairing.Lock()
	defer s.pairing.Unlock()
	psk := s.pending
	s.dropAttempt()
	return psk
}

func (c *Client) protected() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, session := range c.live {
		if session == nil {
			continue
		}
		if id := session.LongTerm(); id != "" {
			out[id] = true
		}
	}
	return out
}

func (c *Client) pairingMessage(session *Session, kind string, payload json.RawMessage,
	name string, once func(string, ...any)) error {

	switch kind {
	case typeServerPairFinalize:
		psk := session.finalized()
		if psk == nil {
			once("sendspin: %q acknowledged a pairing this player was not running", name)
			return nil
		}
		if err := c.PSKs.Records.put(session.peer, psk, c.protected()); err != nil {
			c.Peer.Printf("sendspin: %q paired, and this dot could not keep the record: %v",
				name, err)
			return nil
		}
		c.Peer.Printf("sendspin: paired with %q (server %s)", name, session.serverID)
	case typePairAbort:
		session.endAttempt()
		var a pairAbort
		_ = json.Unmarshal(payload, &a)
		c.Peer.Printf("sendspin: %q ended the pairing attempt: %q", name,
			untrustedlog.Cut(a.Reason))
	case typeServerUnpair:
		id := session.LongTerm()
		if id == "" {
			once("sendspin: %q asked to unpair a session that is not paired", name)
			return nil
		}
		if err := c.PSKs.Records.remove(id); err != nil {
			c.Peer.Printf("sendspin: %q unpaired, and this dot could not drop the record: %v",
				name, err)
		}
		_ = session.Goodbye(goodbyeUnpaired)
		return errUnpaired
	}
	return nil
}

func pairingKind(kind string) bool {
	return slices.Contains([]string{typeServerPairFinalize, typePairAbort, typeServerUnpair}, kind)
}
