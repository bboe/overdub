package sendspin

import (
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/untrustedlog"
)

func delayCommand(ms *int) serverCommand {
	return serverCommand{Player: &playerCommand{Command: commandStaticDelay, StaticDelayMS: ms}}
}

func TestADelayAServerSetsPlacesTheAudioThatMuchEarlier(t *testing.T) {
	const offset = 7_000_000
	const lead = 2 * time.Second
	const delay = 300 * time.Millisecond

	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, offset)

	ms := int(delay / time.Millisecond)
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&ms)))

	playing(t, peer, server)
	waitFor(t, "the player to be handed a stream", func() bool { return player.count() == 1 })

	due := nowMicros() + lead.Microseconds()
	peer.writeBinary(server.seal(t, chunkAt(due+offset, 1200)))
	stream := player.last()
	waitFor(t, "the chunk to reach the stream", func() bool { return stream.wrote() == 1 })

	stream.mu.Lock()
	at := stream.at[0]
	stream.mu.Unlock()

	want := monotonicStart.Add(time.Duration(due) * time.Microsecond).Add(-delay)
	if off := at.Sub(want); off > 20*time.Millisecond || off < -20*time.Millisecond {
		t.Errorf("a chunk was placed %s from where a %s output delay puts it: the delay is"+
			" the latency past this device's audio port, so the audio has to be handed"+
			" over that much earlier to be heard on time", off, delay)
	}
}

func TestTheStateOffersTheDelayCommandSoAServerCanSetItAtAll(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	serveOn(t, c, ln)
	_, _, state := bringUp(t, c, ln)

	if state.Player == nil {
		t.Fatal("client/state carried no player object while the player role is active")
	}
	if len(state.Player.SupportedCommands) != 1 ||
		state.Player.SupportedCommands[0] != commandStaticDelay {
		t.Errorf("client/state offers %v, want just %q: music assistant only shows the"+
			" delay control for a player that names this command, so an empty list is"+
			" a dot whose delay nobody can set", state.Player.SupportedCommands,
			commandStaticDelay)
	}
}

func TestADelayOutsideWhatTheSpecAllowsIsHeldAtTheEndItPassed(t *testing.T) {
	s := &Session{roles: []string{rolePlayerV1}}
	for ms, want := range map[int]time.Duration{
		-1:                   0,
		MaxStaticDelayMS + 1: MaxStaticDelayMS * time.Millisecond,
	} {
		payload, err := json.Marshal(delayCommand(&ms))
		if err != nil {
			t.Fatal(err)
		}
		got, asked, ok, err := s.StaticDelay(payload)
		if err != nil || !ok {
			t.Fatalf("a delay of %d ms came back as (taken=%v, err=%v), and the spec says"+
				" to hold one to the range rather than refuse it", ms, ok, err)
		}
		if got != want || asked != ms {
			t.Errorf("a delay of %d ms was taken as %s asking %d, want %s asking %d",
				ms, got, asked, want, ms)
		}
	}
}

func TestASetStaticDelayCarryingNoDelayIsNotTaken(t *testing.T) {
	s := &Session{roles: []string{rolePlayerV1}}
	payload, err := json.Marshal(delayCommand(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.StaticDelay(payload); err == nil || ok {
		t.Error("set_static_delay with no static_delay_ms was taken, and a missing field" +
			" would otherwise read as the zero it is not")
	}
}

func TestADelayForARoleThisClientDoesNotHoldIsNotTaken(t *testing.T) {
	s := &Session{}
	ms := 300
	payload, err := json.Marshal(delayCommand(&ms))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.StaticDelay(payload); err == nil || ok {
		t.Error("a delay was taken for a client holding no player role, so a server that" +
			" never activated the player could still move where its audio lands")
	}
}

func TestACommandThisPlayerDoesNotTakeIsNotADelay(t *testing.T) {
	s := &Session{roles: []string{rolePlayerV1}}
	for _, body := range []string{
		`{"player":{"command":"volume","volume":50}}`,
		`{"player":{"command":"mute","mute":true}}`,
		`{}`,
	} {
		delay, _, ok, err := s.StaticDelay(json.RawMessage(body))
		if ok || err != nil || delay != 0 {
			t.Errorf("%s came back as delay %s (taken=%v, err=%v), want it left alone for"+
				" the line that says a command is not handled yet", body, delay, ok, err)
		}
	}
}

func TestDraggingTheDelayCostsTheLogOneLineRatherThanOnePerValue(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for ms := 1200; ms > 1100; ms -= 5 {
		set := ms
		peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&set)))
	}
	last := 1100
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&last)))
	syncClock(t, peer, server, 0)

	if got := strings.Count(out.String(), "is setting this player's output delay"); got != 1 {
		t.Errorf("21 values in a row wrote %d lines, want 1: music assistant applies this"+
			" control as it is dragged, so a line per value spends the peer log's whole"+
			" minute and suppresses the summaries that say what the audio did", got)
	}
}

func TestASummarySaysWhatDelayItWasPlacedAgainst(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery,
		play: &playback{stream: &fakeStream{},
			held: func() time.Duration { return 1097 * time.Millisecond }}}
	feedChunks(t, &run, peer, s, 2)
	run.report(peer, "server")

	if !strings.Contains(out.String(), "placed 1.097s earlier than stamped") {
		t.Errorf("the summary never says the delay it placed against, so the one line"+
			" that is not rate limited by a server carries no way to see what it set:\n%s",
			out.String())
	}
}

func TestADelayBeyondWhatTheLeadCarriesIsHeldAtWhatItCanAndReportedBack(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	asked := MaxStaticDelayMS + 2000
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))

	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("a delay this player cannot hold was answered with %s, want a %s"+
			" correcting it: the server keeps its own copy of this figure and music"+
			" assistant shows it, so a value we do not apply is a slider that lies",
			kind, typeClientState)
	}
	var state clientState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.Player == nil || state.Player.StaticDelayMS != MaxStaticDelayMS {
		t.Errorf("reported %v back, want the %d ms the spec holds it to", state.Player,
			MaxStaticDelayMS)
	}
}

func TestEveryDelayTakenIsReportedBackSoAServerSchedulesForIt(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	asked := 1814
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))

	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("a delay well inside the range was answered with %s rather than a %s"+
			" carrying it: a server sends ahead by min_buffer_ms plus the delay it last"+
			" read in client/state, so a delay applied and never reported leaves every"+
			" chunk arriving too late to place", kind, typeClientState)
	}
	var state clientState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.Player == nil || state.Player.StaticDelayMS != asked {
		t.Errorf("reported %v back, want the %d ms just taken", state.Player, asked)
	}
}

func TestADelaySetAgainToWhatItAlreadyIsSaysNothing(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	asked := 900
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientState {
		t.Fatalf("wanted the %s carrying the delay, got %s", typeClientState, kind)
	}
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))

	if !noStateWithin(t, peer, server, 300*time.Millisecond) {
		t.Error("the same delay sent twice reported twice, so a server that re-asserts" +
			" what it already set is answered every time for nothing")
	}
}

func noStateWithin(t *testing.T, peer *wsPeer, server *serverSide, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return true
		}
		if err := peer.conn.SetReadDeadline(time.Now().Add(left)); err != nil {
			t.Fatal(err)
		}
		_, err := peer.r.Peek(1)
		if err := peer.conn.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			return true
		}
		if kind, _ := readJSON(t, peer, server); kind == typeClientState {
			return false
		}
	}
}

func TestADelayIsRememberedAcrossTheNextConnection(t *testing.T) {
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
	kept := func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(saved)
	}
	serveOn(t, c, ln)

	peer, server, _ := bringUp(t, c, ln)
	asked := 900
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))
	waitFor(t, "the delay to be kept", func() bool { return len(kept()) == 1 })
	if got := kept(); got[0] != asked {
		t.Fatalf("kept %d ms, want %d", got[0], asked)
	}
	_ = peer.conn.Close()

	_, _, state := bringUp(t, c, ln)
	if state.Player == nil || state.Player.StaticDelayMS != asked {
		t.Errorf("the next connection reported %v, want the %d ms the last one set: the"+
			" spec says a client keeps this across reconnections, and a server that"+
			" reconnects without resending it would otherwise play %d ms out",
			state.Player, asked, asked)
	}
}

func TestADelayKeptFromAnEarlierRunIsWhatTheFirstStateCarries(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.DelayMS = 700
	serveOn(t, c, ln)

	_, _, state := bringUp(t, c, ln)
	if state.Player == nil || state.Player.StaticDelayMS != 700 {
		t.Errorf("a dot that kept 700 ms across a reboot reported %v, so the delay the"+
			" spec says to persist is read back and then never used", state.Player)
	}
}

func savingClient(t *testing.T) (*Client, *fakePlayer, func() []int) {
	t.Helper()
	c, player := playingClient(t)
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		saved = append(saved, ms)
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	return c, player, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(saved)
	}
}

func TestADelayAServerSetsToZeroIsStillZeroOnTheNextConnection(t *testing.T) {
	ln := listenLocal(t)
	c, _, kept := savingClient(t)
	c.DelayMS = 1000
	c.keepEvery = 50 * time.Millisecond
	serveOn(t, c, ln)

	peer, server, first := bringUp(t, c, ln)
	if first.Player == nil || first.Player.StaticDelayMS != 1000 {
		t.Fatalf("the first connection reported %v, want the 1000 ms kept from before",
			first.Player)
	}
	zero := 0
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&zero)))
	waitFor(t, "the zero to be kept", func() bool {
		got := kept()
		return len(got) > 0 && got[len(got)-1] == 0
	})
	_ = peer.conn.Close()

	_, _, next := bringUp(t, c, ln)
	if next.Player == nil || next.Player.StaticDelayMS != 0 {
		t.Errorf("the next connection reported %v after a server set the delay to zero:"+
			" zero is a delay a dot with no external amp is meant to have, and treating"+
			" it as nothing brings the old figure back and plays that much early",
			next.Player)
	}
}

func TestADragIsKeptWhenItSettlesRatherThanAtEveryStep(t *testing.T) {
	ln := listenLocal(t)
	c, player, kept := savingClient(t)
	c.keepEvery = 2 * time.Second
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	steps := 0
	for ms := 1200; ms >= 1100; ms -= 5 {
		set := ms
		peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&set)))
		steps++
	}
	playing(t, peer, server)
	waitFor(t, "the stream to reach the player", func() bool { return player.count() == 1 })

	waitFor(t, "the settled delay to be kept", func() bool {
		got := kept()
		return len(got) > 0 && got[len(got)-1] == 1100
	})
	if got := kept(); len(got) > 2 {
		t.Errorf("%d values wrote the property %d times, and a drag inside one window is"+
			" meant to cost one: a write is a measured 40 ms of setprop here, and every"+
			" value but the one somebody stops on is a write nothing will ever read",
			steps, len(got))
	}
}

func TestAFigureTakenAsTheKeeperStopsIsNotOverwrittenByItsLastWrite(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = time.Hour
	writing := make(chan struct{})
	release := make(chan struct{})
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	var mu sync.Mutex
	var saved []int
	var arm atomic.Bool
	arm.Store(true)
	c.SaveDelay = func(ms int) error {
		if arm.CompareAndSwap(true, false) {
			close(writing)
			<-release
		}
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
	waitFor(t, "the keeper to be running", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.keepWake != nil
	})

	c.SetDelay(700)
	ln.Close()
	<-writing

	set := make(chan struct{})
	go func() {
		defer close(set)
		c.SetDelay(900)
	}()
	waitFor(t, "the newer figure to be the one this client holds",
		func() bool { return c.Delay() == 900 })
	free()
	<-set
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("this client was still serving five seconds after its listener closed")
	}

	got := kept()
	if len(got) == 0 || got[len(got)-1] != 900 {
		t.Errorf("the property was written %v, and the last write is what a reboot"+
			" reads: a figure taken while the keeper is stopping raced its last write"+
			" and lost, so flash holds a delay this player had already moved off", got)
	}
}

func TestADelaySetInsideTheWindowIsKeptWhenThePlayerStops(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 10 * time.Second
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		time.Sleep(200 * time.Millisecond)
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
	peer, server, _ := bringUp(t, c, ln)

	asked := 900
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))
	if kind, _ := nextJSON(t, peer, server); kind != typeClientState {
		t.Fatalf("wanted the %s carrying the delay, got %s", typeClientState, kind)
	}
	_ = peer.conn.Close()
	ln.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("this client was still serving five seconds after its listener closed")
	}

	if got := kept(); len(got) != 1 || got[0] != asked {
		t.Errorf("the property held %v by the time this client stopped serving, want the"+
			" %d ms set inside the window: the switch rebuilds the client by reading the"+
			" property the moment it is turned back on, so a write that is merely under"+
			" way reverts the figure in front of whoever set it", got, asked)
	}
}

func TestEachDelayGetsItsOwnAttemptsAtThePropertyRatherThanTheRunsLeftovers(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 20 * time.Millisecond
	var mu sync.Mutex
	var tried []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		tried = append(tried, ms)
		return errors.New("did not read back")
	}
	count := func(ms int) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, got := range tried {
			if got == ms {
				n++
			}
		}
		return n
	}
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	first := 400
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&first)))
	waitFor(t, "the first figure to be refused once", func() bool { return count(first) > 0 })
	second := 800
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&second)))

	waitFor(t, "the second figure to spend its own attempts", func() bool {
		return count(second) >= KeepTries
	})
	keeperCaughtUp(t, c)
	if got := count(second); got != KeepTries {
		t.Errorf("a figure that arrived after another had already been refused was"+
			" written %d times, want %d: the attempts are what one figure is worth, and"+
			" counting them per run spends an earlier figure's failures on this one",
			got, KeepTries)
	}
}

func TestADelayThePropertyRefusedOnceIsWrittenAgain(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
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
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	asked := 900
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))

	waitFor(t, "the refused figure to be written on a later window", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) > 0 && saved[0] == asked
	})
}

func TestADelayThePropertyKeepsRefusingIsGivenUpOn(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 20 * time.Millisecond
	var mu sync.Mutex
	tried := 0
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		tried++
		return errors.New("did not read back")
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return tried
	}
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	asked := 900
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))

	waitFor(t, "the writes to be given up on", func() bool { return count() >= KeepTries })
	keeperCaughtUp(t, c)
	if got := count(); got != KeepTries {
		t.Errorf("a property that refuses every write was written %d times in %d windows,"+
			" want the %d this keeper gives up after: a figure that cannot be stored is"+
			" not worth a setprop a minute for the rest of the boot", got, 20, KeepTries)
	}
}

func TestADelayThatCouldNotBeReadIsWrittenRatherThanAssumed(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 10 * time.Second
	c.DelayUnknown = true
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, ms)
		return nil
	}
	serveOn(t, c, ln)

	waitFor(t, "the figure this player reports to reach the property", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) > 0 && saved[0] == 0
	})
}

func TestADelayWrittenOnceIsNotWrittenAgainWhenThePlayerStops(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.keepEvery = 50 * time.Millisecond
	c.DelayUnknown = true
	var mu sync.Mutex
	var saved []int
	c.SaveDelay = func(ms int) error {
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
	peer, server, _ := bringUp(t, c, ln)

	asked := 700
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&asked)))
	waitFor(t, "the figure to reach the property", func() bool {
		got := kept()
		return len(got) > 0 && got[len(got)-1] == asked
	})
	_ = peer.conn.Close()
	ln.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("this client was still serving five seconds after its listener closed")
	}
	waitForConns(t, c, 0, "the connection was still being served after this client stopped")

	if got := kept(); len(got) != 2 {
		t.Errorf("the unread figure and the one a server set were written %v, want the"+
			" two of them: stopping spends another setprop on a property that already"+
			" holds the figure, and what a keeper has written is what the disk has --"+
			" it is only the startup read that it cannot know", got)
	}
}

func TestManyChangesInsideOneWindowCostOneWriteAtTheEndOfIt(t *testing.T) {
	ln := listenLocal(t)
	c, _, kept := savingClient(t)
	c.keepEvery = 400 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for _, ms := range []int{300, 600, 900, 1200} {
		set := ms
		peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&set)))
		time.Sleep(40 * time.Millisecond)
	}
	if got := kept(); len(got) != 0 {
		t.Fatalf("the property was written %v while the values were still arriving, and"+
			" the whole point of the window is that a control somebody is dragging"+
			" reaches flash once rather than once a step", got)
	}
	waitFor(t, "the window to close and the last value to be kept", func() bool {
		got := kept()
		return len(got) == 1 && got[0] == 1200
	})
}

func TestADelayAlreadyOnDiskIsNotWrittenAgain(t *testing.T) {
	ln := listenLocal(t)
	c, _, kept := savingClient(t)
	c.DelayMS = 700
	c.keepEvery = 500 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	same := 700
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&same)))
	other := 800
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&other)))
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&same)))

	handled(t, peer, server)
	keeperCaughtUp(t, c)
	if got := kept(); len(got) != 0 {
		t.Errorf("a delay that ended where the disk already had it wrote %v: a server"+
			" that re-asserts its own figure, or an operator who nudges a control and"+
			" puts it back, should cost no flash at all", got)
	}
}
