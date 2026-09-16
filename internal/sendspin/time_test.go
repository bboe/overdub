package sendspin

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func answerWith(t *testing.T, k *clock, asked, received, transmitted, at int64) answer {
	t.Helper()
	k.mu.Lock()
	k.asked, k.awaiting = asked, true
	k.mu.Unlock()

	payload, err := json.Marshal(serverTime{
		ClientTransmitted: asked,
		ServerReceived:    received,
		ServerTransmitted: transmitted,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := k.observe(payload, at)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return got
}

func TestTwoAnsweredExchangesConvergeTheClock(t *testing.T) {
	const offset = 4_000_000
	k := newClock()
	for _, asked := range []int64{1_000_000, 2_000_000} {
		if got := answerWith(t, k, asked, asked+offset, asked+offset, asked+2_000); got != measured {
			t.Fatalf("the answer to the question asked at %d was not a measurement", asked)
		}
	}
	if !k.filter.Converged() {
		t.Fatal("two answered exchanges left the clock unconverged")
	}
	if got := k.filter.ServerTime(3_000_000); got < 3_000_000+offset-3_000 ||
		got > 3_000_000+offset+3_000 {
		t.Errorf("server time %d is not within 3 ms of %d", got, 3_000_000+offset)
	}
}

func TestAnAnswerToAQuestionThatWasNeverAskedIsNoMeasurement(t *testing.T) {
	const offset = 4_000_000
	k := newClock()
	for _, asked := range []int64{1_000_000, 2_000_000} {
		k.mu.Lock()
		k.asked, k.awaiting = asked, true
		k.mu.Unlock()
		payload, err := json.Marshal(serverTime{
			ClientTransmitted: asked + 1,
			ServerReceived:    asked + offset,
			ServerTransmitted: asked + offset,
		})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got, err := k.observe(payload, asked+2_000)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if got != unasked {
			t.Fatal("a server/time carrying a stamp this client never sent was measured")
		}
	}
	if k.filter.Converged() {
		t.Error("a server it never asked can hand this clock any offset it likes")
	}
}

func TestAnAnswerRepeatedIsNotASecondMeasurement(t *testing.T) {
	const offset = 4_000_000
	k := newClock()
	if got := answerWith(t, k, 1_000_000, 1_000_000+offset, 1_000_000+offset, 1_002_000); got != measured {
		t.Fatal("the first answer was not a measurement")
	}
	payload, err := json.Marshal(serverTime{
		ClientTransmitted: 1_000_000,
		ServerReceived:    1_000_000 + offset,
		ServerTransmitted: 1_000_000 + offset,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := k.observe(payload, 1_003_000)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if got != unasked {
		t.Error("the same answer counted twice, so one exchange can converge the clock")
	}
}

func TestAnAnswerThatCameBackBeforeItWasSentIsNoMeasurement(t *testing.T) {
	k := newClock()
	for _, asked := range []int64{1_000_000, 2_000_000} {
		got := answerWith(t, k, asked, asked+4_000_000, asked+4_010_000, asked+2_000)
		if got == measured {
			t.Fatal("an exchange the server claims took longer than the round trip was measured")
		}
		if got != spent {
			t.Errorf("the answer reads as %q, so a well-formed reply to a question this"+
				" client did ask is reported as something else", got)
		}
	}
	if k.filter.Converged() {
		t.Error("a negative round-trip delay reached the filter as an uncertainty")
	}
}

func TestAnAnswerWhoseStampsAreNotOnAClockIsNoMeasurement(t *testing.T) {
	k := newClock()
	for _, asked := range []int64{1_000_000, 2_000_000} {
		got := answerWith(t, k, asked, math.MinInt64, math.MaxInt64, asked+2_000)
		if got != offClock {
			t.Fatalf("stamps at the ends of int64 read as %v; their difference wraps to"+
				" a small positive delay and the offset it carries is nonsense", got)
		}
	}
	if k.filter.Converged() {
		t.Error("a wrapped round-trip delay reached the filter as a confident measurement")
	}
}

func TestAnExchangeThatSpentNoTimeOnTheWireIsNoMeasurement(t *testing.T) {
	k := newClock()
	for _, asked := range []int64{1_000_000, 2_000_000} {
		got := answerWith(t, k, asked, asked+4_000_000, asked+4_002_000, asked+2_000)
		if got != spent {
			t.Fatalf("an exchange the server accounts for entirely reads as %v, and a"+
				" measurement of zero uncertainty pins the filter's gain at zero for"+
				" the rest of the connection", got)
		}
	}
	if k.filter.Converged() {
		t.Error("a server can pin this clock wherever it likes by claiming the whole" +
			" round trip as its own processing time")
	}
}

func TestAnAnswerSentBeforeTheQuestionArrivedIsNoMeasurement(t *testing.T) {
	k := newClock()
	for _, asked := range []int64{1_000_000, 2_000_000} {
		got := answerWith(t, k, asked, asked+4_000_000, 0, asked+2_000)
		if got != backwards {
			t.Fatalf("a server that transmitted before it received reads as %q; the"+
				" round trip it did not spend lands in the delay as certainty", got)
		}
	}
	if k.filter.Converged() {
		t.Error("the reference server's own unstamped placeholder would set this clock" +
			" outright, because the first two measurements are taken unweighted")
	}
}

func TestAnUnusableAnswerStillEndsTheWaitForOne(t *testing.T) {
	k := newClock()
	answerWith(t, k, 1_000_000, 5_000_000, 5_010_000, 1_002_000)
	select {
	case <-k.replied:
	default:
		t.Error("an answered question left the loop waiting out its whole answer window")
	}
}

func TestALateAnswerDoesNotShortenTheWaitForTheNextOne(t *testing.T) {
	session, _, peer := pairedSession(t)
	k := session.clock
	k.replied <- struct{}{}

	asked := make(chan error, 1)
	go func() { asked <- k.ask(session) }()
	peer.read()
	if err := <-asked; err != nil {
		t.Fatalf("ask: %v", err)
	}
	select {
	case <-k.replied:
		t.Error("an answer that arrived too late to be waited for survived into the next" +
			" question, so the client asks again without waiting for the answer to it")
	default:
	}
}

func TestTheDotAsksForTheTimeOncePlayerIsActive(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)

	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	if kind, _ := readJSON(t, peer, server); kind != typeClientHello {
		t.Fatalf("wanted %s, got %s", typeClientHello, kind)
	}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	if kind, _ := readJSON(t, peer, server); kind != typeClientState {
		t.Fatalf("wanted %s, got %s", typeClientState, kind)
	}

	kind, payload := readJSON(t, peer, server)
	if kind != typeClientTime {
		t.Fatalf("wanted %s, got %s", typeClientTime, kind)
	}
	var ask clientTime
	if err := json.Unmarshal(payload, &ask); err != nil {
		t.Fatalf("decoding client/time: %v", err)
	}
	if ask.ClientTransmitted <= 0 {
		t.Errorf("client_transmitted is %d, so the server has no stamp to echo",
			ask.ClientTransmitted)
	}
}

func TestAnsweredExchangesConvergeTheHeldSession(t *testing.T) {
	const offset = 7_000_000
	ln := listenLocal(t)
	c := testClient(t)
	c.timeEvery = 10 * time.Millisecond
	c.answerAfter = 50 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for range 3 {
		kind, payload := readJSON(t, peer, server)
		if kind != typeClientTime {
			t.Fatalf("wanted %s, got %s", typeClientTime, kind)
		}
		var ask clientTime
		if err := json.Unmarshal(payload, &ask); err != nil {
			t.Fatalf("decoding client/time: %v", err)
		}
		at := ask.ClientTransmitted + offset
		peer.writeBinary(server.sealJSON(t, typeServerTime, serverTime{
			ClientTransmitted: ask.ClientTransmitted,
			ServerReceived:    at,
			ServerTransmitted: at,
		}))
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		held := c.held
		c.mu.Unlock()
		if held != nil && held.clock.filter.Converged() {
			got := held.clock.filter.ServerTime(0)
			if got < offset-100_000 || got > offset+100_000 {
				t.Errorf("the session puts the server %d us away, want about %d", got, offset)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the exchanges were answered and the clock never converged")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
