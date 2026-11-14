package sendspin

import (
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func elsewhereClient(t *testing.T) (*Client, *atomic.Bool) {
	t.Helper()
	c, _ := outputClient(t, StreamRate)
	away := &atomic.Bool{}
	c.Config.Elsewhere = away.Load
	return c, away
}

func nextAvailable(t *testing.T, peer *wsPeer, server *serverSide) bool {
	t.Helper()
	for {
		kind, payload := nextJSON(t, peer, server)
		if kind != typeClientState {
			continue
		}
		var s clientState
		if err := json.Unmarshal(payload, &s); err != nil {
			t.Fatalf("decoding %s: %v", typeClientState, err)
		}
		return s.Available
	}
}

func syncedOn(t *testing.T, c *Client, ln net.Listener) (*wsPeer, *serverSide, bool) {
	t.Helper()
	serveOn(t, c, ln)
	peer, server, first := bringUp(t, c, ln)
	if first.Available {
		t.Fatal("available before the clock had agreed with the server's")
	}
	syncClock(t, peer, server, 0)
	return peer, server, nextAvailable(t, peer, server)
}

func TestAlexaPlayingTakesTheDotOutOfPlayAndBack(t *testing.T) {
	c, away := elsewhereClient(t)
	peer, server, available := syncedOn(t, c, listenLocal(t))
	if !available {
		t.Fatal("a synced dot with nothing else playing reported itself unavailable")
	}

	away.Store(true)
	if nextAvailable(t, peer, server) {
		t.Fatal("alexa started playing and the dot still said it was available: the server" +
			" keeps streaming, and both play at full level")
	}
	away.Store(false)
	if !nextAvailable(t, peer, server) {
		t.Error("alexa stopped and the dot never said it was available again, so no server" +
			" can play on it")
	}
}

func TestADotAlexaIsAlreadyPlayingOnSyncsUnavailable(t *testing.T) {
	c, away := elsewhereClient(t)
	away.Store(true)
	peer, server, available := syncedOn(t, c, listenLocal(t))
	if available {
		t.Fatal("the clock agreed while alexa played, and the dot said it was available")
	}

	away.Store(false)
	if !nextAvailable(t, peer, server) {
		t.Error("alexa stopped and the dot never said it was available")
	}
}

func TestANudgeTellsTheServerWithoutWaitingForThePoll(t *testing.T) {
	c, away := elsewhereClient(t)
	c.outputEvery = time.Hour
	peer, server, available := syncedOn(t, c, listenLocal(t))
	if !available {
		t.Fatal("a synced dot with nothing else playing reported itself unavailable")
	}

	away.Store(true)
	c.NoteElsewhere()
	if nextAvailable(t, peer, server) {
		t.Fatal("alexa started playing and the nudged dot still said it was available")
	}
	away.Store(false)
	c.NoteElsewhere()
	if !nextAvailable(t, peer, server) {
		t.Error("alexa stopped and the nudged dot never said it was available again")
	}
}

func TestAStateBuiltFromAStaleReadingStillSaysAvailable(t *testing.T) {
	c, _ := elsewhereClient(t)
	c.outputEvery = time.Hour
	peer, server, available := syncedOn(t, c, listenLocal(t))
	if !available {
		t.Fatal("a synced dot with nothing else playing reported itself unavailable")
	}

	session, _ := c.reporting()
	if err := c.state(session, false); err != nil {
		t.Fatal(err)
	}
	if !nextAvailable(t, peer, server) {
		t.Error("a state written with a readiness read before the clock agreed said" +
			" unavailable after the dot had said available, and nothing sends true again")
	}
}
