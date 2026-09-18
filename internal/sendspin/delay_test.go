package sendspin

import (
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"

	"github.com/bboe/overdub/internal/untrustedlog"
)

func delaySet(t *testing.T, peer *wsPeer, server *serverSide) int {
	t.Helper()
	ms, _ := delayState(t, peer, server)
	return ms
}

func delayState(t *testing.T, peer *wsPeer, server *serverSide) (ms int, available bool) {
	t.Helper()
	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("wanted the %s a delay is reported in, got %s", typeClientState, kind)
	}
	var state clientState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.Player == nil {
		t.Fatal("client/state carried no player object, so it carries no delay either")
	}
	return state.Player.StaticDelayMS, state.Available
}

func TestADelayHomeAssistantSetsIsReportedToTheServerAtOnce(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	c.SetDelay(1500)

	got, available := delayState(t, peer, server)
	if available {
		t.Error("a delay set before this player's clock agreed with the server's reported" +
			" the player available, and a state written from another goroutine that" +
			" guesses that flag invites audio this player can only drop")
	}
	if got != 1500 {
		t.Errorf("a delay set in Home Assistant was reported to the server as %d ms, want"+
			" 1500: a server sends ahead by min_buffer_ms plus the delay it last read in"+
			" client/state, so a delay applied here and never reported leaves every"+
			" chunk arriving too late to place", got)
	}
	if got := c.heldDelay(); got != 1500*time.Millisecond {
		t.Errorf("the client holds %s, want 1.5s", got)
	}
}

func TestADelayHomeAssistantSetsIsHeldToTheRangeTheSpecAllows(t *testing.T) {
	for _, tt := range []struct {
		asked int
		want  time.Duration
	}{
		{-1, 0},
		{MaxStaticDelayMS + 1, MaxStaticDelayMS * time.Millisecond},
	} {
		c, _ := playingClient(t)
		c.SetDelay(tt.asked)
		if got := c.heldDelay(); got != tt.want {
			t.Errorf("a delay of %d ms was held at %s, want %s: the spec bounds the"+
				" figure a client applies and reports, whichever end asked for it",
				tt.asked, got, tt.want)
		}
	}
}

func TestADelaySetToZeroIsNotTheFigureTheLastRunKept(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.DelayMS = 700
	serveOn(t, c, ln)
	peer, server, state := bringUp(t, c, ln)
	if state.Player == nil || state.Player.StaticDelayMS != 700 {
		t.Fatalf("the first state reported %v, want the 700 ms kept from the last run",
			state.Player)
	}

	c.SetDelay(0)

	if got := delaySet(t, peer, server); got != 0 {
		t.Errorf("a delay turned off in Home Assistant was reported as %d ms: zero is a"+
			" figure somebody chose rather than the absence of one, and a dot that"+
			" answers with what it kept from the last run cannot be turned off at all",
			got)
	}
	if got := c.heldDelay(); got != 0 {
		t.Errorf("the client still holds %s after being set to zero", got)
	}
}

func TestADelaySetAgainToWhatItAlreadyIsIsNotReportedTwice(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	c.SetDelay(900)
	if got := delaySet(t, peer, server); got != 900 {
		t.Fatalf("the first set reported %d ms, want 900", got)
	}
	c.SetDelay(900)

	if !noStateWithin(t, peer, server, 300*time.Millisecond) {
		t.Error("the same delay set twice was reported twice, so an operator holding a" +
			" control at one figure is answered for nothing")
	}
	waitFor(t, "the change to reach the property", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) > 0
	})
	time.Sleep(4 * c.keepEvery)
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0] != 900 {
		t.Errorf("the property was written %v, want the one write the change needed:"+
			" persisting a figure that has not moved is two forks apiece", saved)
	}
}

func TestADelayHomeAssistantSetsIsPersisted(t *testing.T) {
	c, _ := playingClient(t)
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}

	c.SetDelay(1200)

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0] != 1200 {
		t.Errorf("a delay set with no server connected was persisted as %v, want one"+
			" 1200: the spec says a client keeps this across reboots whoever set it,"+
			" and the next connection reads it from there", saved)
	}
}

func TestADelayHomeAssistantSetsMovesAudioAlreadyMidStream(t *testing.T) {
	const offset = 7_000_000
	const lead = 2 * time.Second
	const set = 400 * time.Millisecond

	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, offset)

	playing(t, peer, server)
	waitFor(t, "the player to be handed a stream", func() bool { return player.count() == 1 })
	stream := player.last()

	first := nowMicros() + lead.Microseconds()
	peer.writeBinary(server.seal(t, chunkAt(first+offset, 1200)))
	waitFor(t, "the first chunk to reach the stream", func() bool { return stream.wrote() == 1 })

	c.SetDelay(int(set / time.Millisecond))

	due := nowMicros() + lead.Microseconds()
	peer.writeBinary(server.seal(t, chunkAt(due+offset, 1200)))
	waitFor(t, "the second chunk to reach the stream", func() bool { return stream.wrote() == 2 })

	stream.mu.Lock()
	at := stream.at[1]
	stream.mu.Unlock()

	want := monotonicStart.Add(time.Duration(due) * time.Microsecond).Add(-set)
	if off := at.Sub(want); off > 20*time.Millisecond || off < -20*time.Millisecond {
		t.Errorf("a chunk that arrived after the delay moved was placed %s from where"+
			" the new %s puts it: the connection took its own copy of the figure when it"+
			" opened, so a delay set from Home Assistant mid-track does nothing until"+
			" the server reconnects", off, set)
	}
}

func TestADelaySetFromHomeAssistantDoesNotTakeThePlayerAway(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, 0)
	waitFor(t, "the player to be reported available", func() bool {
		_, available := c.reporting()
		return available
	})

	c.SetDelay(600)

	kind, payload := nextJSON(t, peer, server)
	for kind == typeClientState {
		var state clientState
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		if state.Player != nil && state.Player.StaticDelayMS == 600 {
			if !state.Available {
				t.Error("the state carrying a delay Home Assistant set reported this" +
					" player unavailable, so moving the control drops the dot out of" +
					" its group until something else says otherwise")
			}
			return
		}
		kind, payload = nextJSON(t, peer, server)
	}
	t.Fatalf("no state carried the delay just set; got %s", kind)
}

func TestADelayTurnedOffBeforeAnyServerArrivesIsStillTurnedOff(t *testing.T) {
	c, _ := playingClient(t)
	c.DelayMS = 700
	var saved []int
	c.SaveDelay = func(ms int) error {
		saved = append(saved, ms)
		return nil
	}

	c.SetDelay(0)

	if got := c.heldDelay(); got != 0 {
		t.Errorf("a dot set to no delay before any server connected holds %s: the figure"+
			" kept from the last run is read when the first one is needed, so a set that"+
			" lands before that read is overwritten by the figure it replaced", got)
	}
	if len(saved) != 1 || saved[0] != 0 {
		t.Errorf("the set was persisted as %v, want one 0", saved)
	}
}

func TestADelaySetAfterThisClientStopsServingIsStillKept(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = c.Serve(ln)
	}()
	waitFor(t, "this client to be listening", func() bool { return c.subject() != "" })
	ln.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("this client was still serving five seconds after its listener closed")
	}

	c.SetDelay(1300)

	waitFor(t, "the figure to reach the property with no keeper left to wake", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) > 0 && saved[len(saved)-1] == 1300
	})
}

func TestADelaySetBeforeThisClientServesSurvivesItStartingUp(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	c.DelayMS = 700
	c.SaveDelay = func(int) error { return nil }

	c.SetDelay(0)
	serveOn(t, c, ln)
	time.Sleep(200 * time.Millisecond)

	if got := c.Delay(); got != 0 {
		t.Errorf("a delay set to %d ms before this client started serving came back as the"+
			" %d ms kept from last time: the figure somebody just chose is the one in"+
			" force, and seeding over it makes the control spring back", got, c.DelayMS)
	}
}

func TestADelaySetWhileThisClientIsStoppingIsStillKept(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = time.Hour
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}
	kept := func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(saved)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = c.Serve(ln)
	}()
	waitFor(t, "this client to be serving", func() bool { return c.subject() != "" })

	c.SetDelay(900)
	ln.Close()
	c.SetDelay(1234)
	<-served

	waitFor(t, "the figure set as this client stopped to reach the property", func() bool {
		return slices.Contains(kept(), 1234)
	})
}
func TestAFigureTheDirectWriteRefusedIsWrittenOnceTheKeeperRuns(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	c.DelayMS = 700
	var mu sync.Mutex
	var saved []int
	refused := false
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		if !refused {
			refused = true
			return errors.New("did not read back")
		}
		saved = append(saved, ms)
		return nil
	}

	c.SetDelay(1100)
	serveOn(t, c, ln)

	waitFor(t, "the refused figure to be written once a keeper is running", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) > 0 && saved[len(saved)-1] == 1100
	})
}

func TestAFigureAlreadyOnDiskIsNotWrittenAgainWhenThisClientStartsServing(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	c.DelayMS = 700
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}

	c.SetDelay(300)
	serveOn(t, c, ln)
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0] != 300 {
		t.Errorf("starting up wrote %v, want the one 300 the set before it wrote: the"+
			" keeper is seeded from what reached the property rather than from what this"+
			" client was built with, so a figure already on disk costs no second write",
			saved)
	}
}

func TestAFigureTheKeeperWroteIsNotWrittenAgainByALaterStart(t *testing.T) {
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	c.DelayMS = 700
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}

	first := listenLocal(t)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = c.Serve(first)
	}()
	waitFor(t, "this client to be listening", func() bool { return c.subject() != "" })

	c.SetDelay(400)
	waitFor(t, "the keeper to write the figure it holds", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) > 0 && saved[len(saved)-1] == 400
	})

	first.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("this client was still serving five seconds after its listener closed")
	}

	mu.Lock()
	before := len(saved)
	mu.Unlock()

	serveOn(t, c, listenLocal(t))
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != before {
		t.Errorf("starting up again wrote %v, want nothing after the first %d: what the"+
			" keeper itself wrote is on disk as surely as a direct write, and seeding the"+
			" next keeper from anything else spends a flash write saying what the property"+
			" already says", saved[before:], before)
	}
}

func TestSettingTheDelayDoesNotWaitOnTheServerSocket(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, 0)
	waitFor(t, "the player to be reported available", func() bool {
		_, available := c.reporting()
		return available
	})

	session, _ := c.reporting()
	session.writing.Lock()
	defer session.writing.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.SetDelay(600)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("setting the delay was still waiting on a server that was not reading:" +
			" this runs on the switch's one worker, and a write held there is a write" +
			" deadline of 150 seconds in which the switch answers nothing at all")
	}
}

func TestADelaySetWhileAServerHoldsNoRoleIsReportedWhenTheRoleComesBack(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, 0)
	waitFor(t, "the player to be reported available", func() bool {
		_, available := c.reporting()
		return available
	})

	c.SetDelay(123)
	for {
		kind, payload := nextJSON(t, peer, server)
		if kind != typeClientState {
			continue
		}
		var state clientState
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		if state.Player != nil && state.Player.StaticDelayMS == 123 {
			break
		}
	}

	none := []string{}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities: []string{}, ActiveRoles: &none,
	}))
	waitFor(t, "this server to hold no role", func() bool {
		held, _ := c.reporting()
		return held == nil
	})

	c.SetDelay(600)

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))

	for {
		kind, payload := nextJSON(t, peer, server)
		if kind != typeClientState {
			continue
		}
		var state clientState
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		if state.Player == nil {
			continue
		}
		if got := state.Player.StaticDelayMS; got != 600 {
			t.Fatalf("the first state after this server took the player role back"+
				" carried %d ms, want the 600 set while it held none: a figure moved"+
				" while a server is between roles is one it never hears about, and it"+
				" schedules against the old one for the rest of the session", got)
		}
		return
	}
}

type closeSpy struct {
	net.Conn
	mu     sync.Mutex
	closed bool
}

func (c *closeSpy) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *closeSpy) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func TestAKeepaliveThisPlayerCannotWriteEndsTheConnection(t *testing.T) {
	dead, far := net.Pipe()
	far.Close()
	t.Cleanup(func() { dead.Close() })

	watched, other := net.Pipe()
	t.Cleanup(func() { other.Close() })
	spy := &closeSpy{Conn: watched}

	c := testClient(t)
	c.Peer = &untrustedlog.Log{Subject: "sendspin"}
	stop := make(chan struct{})
	defer close(stop)

	go c.keepalive(&Conn{c: dead}, spy, stop, 10*time.Millisecond)

	waitFor(t, "the connection to be closed after a ping could not be written",
		spy.isClosed)
}

func TestATimeSyncThisPlayerCannotWriteEndsTheConnection(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.answerAfter = 50 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, 0)

	session, _ := c.reporting()
	if session == nil {
		t.Fatal("no session was held after this server activated the player role")
	}

	watched, other := net.Pipe()
	t.Cleanup(func() { other.Close() })
	spy := &closeSpy{Conn: watched}

	peer.conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go c.keepTime(session, spy, stop)

	waitFor(t, "the connection to be closed after a time sync could not be written",
		spy.isClosed)
}

func sendCipher(t *testing.T) *noise.CipherState {
	t.Helper()
	starter, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: noiseSuite, Pattern: noise.HandshakeNN, Initiator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	answerer, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: noiseSuite, Pattern: noise.HandshakeNN,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _, _, err := starter.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := answerer.ReadMessage(nil, first); err != nil {
		t.Fatal(err)
	}
	second, _, _, err := answerer.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, mine, _, err := starter.ReadMessage(nil, second)
	if err != nil {
		t.Fatal(err)
	}
	return mine
}

func TestADelayReportThisPlayerCannotWriteEndsTheConnection(t *testing.T) {
	dead, far := net.Pipe()
	far.Close()
	t.Cleanup(func() { dead.Close() })

	watched, other := net.Pipe()
	t.Cleanup(func() { other.Close() })
	spy := &closeSpy{Conn: watched}

	c := testClient(t)
	c.Peer = &untrustedlog.Log{Subject: "sendspin"}
	session := &Session{
		ws:     &Conn{c: dead},
		send:   sendCipher(t),
		clock:  newClock(),
		report: make(chan struct{}, 1),
	}
	if err := c.hold(session, true); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go c.reportDelays(session, spy, stop)

	c.SetDelay(600)

	waitFor(t, "the connection to be closed after a report could not be written",
		spy.isClosed)
}
