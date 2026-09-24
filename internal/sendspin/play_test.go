package sendspin

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStream struct {
	mu        sync.Mutex
	at        []time.Time
	frames    []int
	pcm       []byte
	cleared   int
	closed    int
	finished  int
	resumed   int
	exhausted bool
	placed    int64
	quiet     int64
}

func (s *fakeStream) Write(at time.Time, pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = append(s.at, at)
	s.frames = append(s.frames, len(pcm)/frameBytes)
	s.pcm = append(s.pcm, pcm...)
	return nil
}

func (s *fakeStream) audio() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pcm)
}

func (s *fakeStream) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared++
}

func (s *fakeStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
}

func (s *fakeStream) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
}

func (s *fakeStream) Resume() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exhausted {
		return false
	}
	s.resumed++
	return true
}

func (s *fakeStream) Placed() (audio, silence int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.placed, s.quiet
}

func (s *fakeStream) heard(audio, silence int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placed, s.quiet = audio, silence
}

func (s *fakeStream) Spent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exhausted
}

func (s *fakeStream) ended() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

func (s *fakeStream) carriedOn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumed
}

func (s *fakeStream) exhaust() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exhausted = true
}

func (s *fakeStream) wrote() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.at)
}

func (s *fakeStream) shut() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeStream) flushed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleared
}

type fakePlayer struct {
	mu      sync.Mutex
	opened  []*fakeStream
	refusal error
}

func (p *fakePlayer) OpenStream(func(string, ...any)) (Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refusal != nil {
		return nil, p.refusal
	}
	s := &fakeStream{}
	p.opened = append(p.opened, s)
	return s, nil
}

func (p *fakePlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.opened)
}

func (p *fakePlayer) last() *fakeStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.opened) == 0 {
		return nil
	}
	return p.opened[len(p.opened)-1]
}

func playingClient(t *testing.T) (*Client, *fakePlayer) {
	t.Helper()
	c := testClient(t)
	c.timeEvery = 10 * time.Millisecond
	c.answerAfter = time.Second
	player := &fakePlayer{}
	c.Player = player
	return c, player
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func syncClock(t *testing.T, peer *wsPeer, server *serverSide, offset int64) {
	t.Helper()
	for range 2 {
		kind, payload := readJSON(t, peer, server)
		for kind == typeClientState {
			kind, payload = readJSON(t, peer, server)
		}
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
}

func chunkAt(stamp int64, samples int) []byte {
	b := make([]byte, 1+chunkStampBytes+samples*frameBytes)
	b[0] = binaryAudioChunk
	binary.BigEndian.PutUint64(b[1:1+chunkStampBytes], uint64(stamp))
	return b
}

func TestAChunkIsPlacedAtTheClientTimeTheFilterGives(t *testing.T) {
	const offset = 7_000_000
	const lead = 500 * time.Millisecond

	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, offset)

	playing(t, peer, server)
	waitFor(t, "the player to be handed a stream", func() bool { return player.count() == 1 })

	due := nowMicros() + lead.Microseconds()
	peer.writeBinary(server.seal(t, chunkAt(due+offset, 1200)))
	stream := player.last()
	waitFor(t, "the chunk to reach the stream", func() bool { return stream.wrote() == 1 })

	stream.mu.Lock()
	at, frames := stream.at[0], stream.frames[0]
	stream.mu.Unlock()

	want := monotonicStart.Add(time.Duration(due) * time.Microsecond)
	if off := at.Sub(want); off > 20*time.Millisecond || off < -20*time.Millisecond {
		t.Errorf("a chunk stamped %d us on a clock %s ahead of ours was placed %s from"+
			" where that puts it; the server's own clock reached the player rather than"+
			" this end's reading of it", due+offset, micros(offset), off)
	}
	if frames != 1200 {
		t.Errorf("the player was handed %d frames, want the 1,200 the chunk carried", frames)
	}
}

func TestAPlayerWithSomewhereToPlayReportsItselfAvailable(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, first := bringUp(t, c, ln)
	if first.Available {
		t.Error("available before the clock had agreed with the server's")
	}
	syncClock(t, peer, server, 0)

	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("wanted a second %s once the clock converged, got %s", typeClientState, kind)
	}
	var state clientState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("decoding client/state: %v", err)
	}
	if !state.Available {
		t.Error("a player with a converged clock and an audio path to play into still" +
			" reports itself unavailable, so no server ever sends it a stream")
	}
}

func TestADotWithNoPlayerStaysUnavailable(t *testing.T) {
	ln := listenLocal(t)
	c := testClient(t)
	c.timeEvery = 10 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)
	syncClock(t, peer, server, 0)

	kind, payload := nextJSON(t, peer, server)
	if kind != typeClientState {
		t.Fatalf("wanted %s, got %s", typeClientState, kind)
	}
	var state clientState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("decoding client/state: %v", err)
	}
	if state.Available {
		t.Error("a dot whose player would not open reported itself available, so a server" +
			" sends it audio that goes nowhere while the group waits for it")
	}
}

func TestTheDeclaredLeadIsWhatTheBufferNeeds(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	c.RequiredLeadMS = 350
	serveOn(t, c, ln)
	_, _, state := bringUp(t, c, ln)
	if state.Player.RequiredLeadTimeMS != 350 {
		t.Errorf("required_lead_time_ms = %d, want the 350 this client was built with:"+
			" a server plans how far ahead to deliver from it, and a zero says the"+
			" first chunk may arrive at the moment it is due",
			state.Player.RequiredLeadTimeMS)
	}
}

func TestEndingAStreamLeavesTheSourceForTheNextOne(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	stream := player.last()

	peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 2}))
	waitFor(t, "the stream to be ended", func() bool { return stream.ended() == 1 })
	if stream.shut() != 0 {
		t.Error("stream/end closed the player's stream rather than ending it. The" +
			" audio it held is discarded either way, but closing gives the source up," +
			" so a stream/start arriving before the writer retires it pays 134 to" +
			" 164 ms of silence learning a mapping it already had")
	}
}

func TestAStreamStartingAgainCarriesOnWhereTheLastOneLeftOff(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	stream := player.last()

	peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 2}))
	waitFor(t, "the stream to be ended", func() bool { return stream.ended() == 1 })

	playing(t, peer, server)
	waitFor(t, "the stream to take audio again", func() bool { return stream.carriedOn() == 1 })
	if player.count() != 1 {
		t.Errorf("%d streams were opened over a track change; the one that has not been"+
			" retired yet holds the mapping, and a new one spends 150 ms of silence"+
			" learning it again", player.count())
	}
}

func TestAPlayerStreamThatHasPlayedItselfOutIsReplaced(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	player.last().exhaust()

	playing(t, peer, server)
	waitFor(t, "a second stream", func() bool { return player.count() == 2 })
}

func TestASecondStreamStartDoesNotOpenASecondStream(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	playing(t, peer, server)
	handled(t, peer, server)

	if player.count() != 1 {
		t.Errorf("%d streams were opened over two stream/starts; the player holds one, so"+
			" the second is refused and the track that follows is silent", player.count())
	}
	if player.last().shut() != 0 {
		t.Error("a stream/start for the track already playing closed the stream under it," +
			" which is a cold start and a fresh 200 ms of silence per track")
	}
}

func TestClearingAStreamThrowsAwayWhatThePlayerHeld(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	stream := player.last()

	peer.writeBinary(server.sealJSON(t, typeStreamClear, streamRoles{ServerTransmitted: 2}))
	waitFor(t, "the buffer to be thrown away", func() bool { return stream.flushed() == 1 })
	if stream.shut() != 0 {
		t.Error("a clear closed the stream; the stream is still the one the server" +
			" announced and the audio after the clear carries on in it")
	}
}

func TestGivingUpThePlayerRoleTakesTheStreamWithIt(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	stream := player.last()

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: &[]string{},
	}))
	waitFor(t, "the stream to be closed", func() bool { return stream.shut() == 1 })
}

func TestAConnectionEndingTakesTheStreamWithIt(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	stream := player.last()

	peer.conn.Close()
	waitFor(t, "the stream to be closed", func() bool { return stream.shut() == 1 })
}

func TestAFormatThisPlayerCannotTakeOpensNoStream(t *testing.T) {
	ln := listenLocal(t)
	c, player := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	stream := player.last()

	peer.writeBinary(server.sealJSON(t, typeStreamStart, streamStart{
		ServerTransmitted: 1, Player: &streamPlayer{
			Codec: codecPCM, SampleRate: 44100,
			Channels: StreamChannels, BitDepth: StreamBitDepth,
		}}))
	peer.writeBinary(server.sealJSON(t, typeGroupUpdate, groupUpdate{GroupName: "after"}))
	waitFor(t, "the refused stream to close the one it replaced", func() bool {
		return stream.shut() == 1
	})

	if player.count() != 1 {
		t.Error("a stream in a format this client never advertised was opened anyway;" +
			" played as pcm 48 kHz it is noise or the right audio at the wrong pitch")
	}
}

func TestAudioBeforeTheClockAgreesIsDroppedAndSaidOnce(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c, player := playingClient(t)
	c.timeEvery = time.Hour
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	playing(t, peer, server)
	waitFor(t, "a stream to be opened", func() bool { return player.count() == 1 })
	for range 5 {
		peer.writeBinary(server.seal(t, chunkAt(nowMicros()+100_000, 1200)))
	}
	handled(t, peer, server)

	if got := player.last().wrote(); got != 0 {
		t.Errorf("%d chunks were placed against a clock that had not converged; the"+
			" stamps are on the server's monotonic clock, so scheduling against them"+
			" puts every frame decades away", got)
	}
	if got := strings.Count(out.String(), "no moment of ours"); got != 1 {
		t.Errorf("five chunks with no clock to place them drew %d lines, want one; at"+
			" forty chunks a second a line each spends the whole run's budget in two"+
			" minutes", got)
	}
}

func TestAPlayerThatWillNotOpenAStreamIsSaidOnce(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c, player := playingClient(t)
	player.refusal = errors.New("a stream is already open")
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	for range 4 {
		playing(t, peer, server)
		peer.writeBinary(server.sealJSON(t, typeStreamEnd, streamRoles{ServerTransmitted: 2}))
	}
	handled(t, peer, server)

	if got := strings.Count(out.String(), "would not open a stream"); got != 1 {
		t.Errorf("a player that refuses every stream drew %d lines over four tracks,"+
			" want one", got)
	}
}
