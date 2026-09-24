package sendspin

import (
	"encoding/binary"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/untrustedlog"
)

func chunkBody(stamp int64, pcm []byte) []byte {
	body := make([]byte, chunkStampBytes, chunkStampBytes+len(pcm))
	binary.BigEndian.PutUint64(body, uint64(stamp))
	return append(body, pcm...)
}

func TestAChunkCarriesItsTimestampBigEndianAheadOfTheAudio(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	c, err := parseChunk(chunkBody(987_654_321, pcm))
	if err != nil {
		t.Fatalf("parseChunk: %v", err)
	}
	if c.ServerTime != 987_654_321 {
		t.Errorf("the timestamp read back as %d; the header is big-endian while the"+
			" audio after it is little-endian", c.ServerTime)
	}
	if string(c.PCM) != string(pcm) {
		t.Errorf("the audio came back as %v, want %v", c.PCM, pcm)
	}
	if c.Frames() != 2 {
		t.Errorf("%d bytes is %d frames, want 2", len(c.PCM), c.Frames())
	}
}

func TestAChunkTooShortToHoldATimestampIsRefused(t *testing.T) {
	for n := range chunkStampBytes {
		if _, err := parseChunk(make([]byte, n)); err == nil {
			t.Errorf("a %d-byte chunk was read for an 8-byte timestamp, which slices past"+
				" the end and panics the daemon into the supervisor's restart loop", n)
		}
	}
}

func TestAChunkThatIsNotWholeFramesIsRefused(t *testing.T) {
	for _, n := range []int{3, 2, 6, 10} {
		if _, err := parseChunk(chunkBody(1, make([]byte, n))); err == nil {
			t.Errorf("%d bytes were accepted, which is not whole %d-byte frames, and every"+
				" frame after it in the stream is built from the wrong channels", n, frameBytes)
		}
	}
}

func TestAChunkCarryingNoAudioIsStillAChunk(t *testing.T) {
	c, err := parseChunk(chunkBody(5, nil))
	if err != nil {
		t.Fatalf("parseChunk: %v", err)
	}
	if c.Frames() != 0 {
		t.Errorf("an empty chunk reported %d frames", c.Frames())
	}
}

func TestAChunkArrivingWithNoStreamOpenIsIgnored(t *testing.T) {
	s := held()
	c, err := s.AudioChunk(chunkBody(1, []byte{1, 2}))
	if err != nil {
		t.Fatalf("AudioChunk: %v", err)
	}
	if c != nil {
		t.Error("audio was taken before any stream/start, so its format is whatever the" +
			" last stream used or nothing at all")
	}
}

func TestAChunkArrivingOnAnOpenStreamIsRead(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)
	c, err := s.AudioChunk(chunkBody(7, []byte{1, 2, 3, 4}))
	if err != nil {
		t.Fatalf("AudioChunk: %v", err)
	}
	if c == nil {
		t.Fatal("audio on an open stream was dropped")
	}
	if c.ServerTime != 7 {
		t.Errorf("the timestamp came back as %d, want 7", c.ServerTime)
	}
}

func TestOnlyTheFirstOfThePlayersBinaryTypesCarriesAudio(t *testing.T) {
	if !playerBinary(binaryAudioChunk) {
		t.Error("the audio chunk is not in the player's own range of binary types")
	}
	for kind := binaryPlayerFirst; kind <= binaryPlayerLast; kind++ {
		if !playerBinary(kind) {
			t.Errorf("%#x is in the player's reserved range and was not read as one", kind)
		}
	}
	if playerBinary(binaryPlayerLast + 1) {
		t.Errorf("%#x belongs to another role and was read as the player's",
			binaryPlayerLast+1)
	}
	if playerBinary(binaryPlayerFirst - 1) {
		t.Errorf("%#x is below the player's range and was read as the player's",
			binaryPlayerFirst-1)
	}
}

func TestASessionWithNoClockYetReportsNoLead(t *testing.T) {
	if _, _, _, ok := (&Session{}).Lead(1); ok {
		t.Error("a lead was reported from a session with no clock, which reads a nil" +
			" filter and panics the daemon into the supervisor's restart loop")
	}
}

func TestAnUnconvergedClockReportsNoLead(t *testing.T) {
	s := &Session{clock: newClock()}
	if _, _, _, ok := s.Lead(1); ok {
		t.Error("a lead was reported from a clock that has not converged, so the number" +
			" logged is the offset of an unset filter rather than a measurement")
	}
}

func TestAChunkDueOffAnyClockThisPlayerKeepsIsRefused(t *testing.T) {
	for _, stamp := range []int64{-1, stampCeiling + 1, math.MinInt64, math.MaxInt64} {
		if _, err := parseChunk(chunkBody(stamp, []byte{1, 2})); err == nil {
			t.Errorf("a chunk due at %d us was accepted, and scheduling against it puts"+
				" audio years away or in the past", stamp)
		}
	}
}

func TestAChunkRunCountsWhatArrivedAndStartsOverAfterReporting(t *testing.T) {
	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	var run chunkRun
	for range 3 {
		c, err := s.AudioChunk(chunkBody(1, make([]byte, 4)))
		if err != nil {
			t.Fatalf("AudioChunk: %v", err)
		}
		run.took(peer, "server", s, c)
	}
	if run.chunks != 3 || run.frames != 3 || run.bytes != 12 {
		t.Errorf("the run counted %d chunks, %d frames, %d bytes; want 3, 3, 12",
			run.chunks, run.frames, run.bytes)
	}

	run.report(peer, "server")
	if run.chunks != 0 {
		t.Error("the run kept its counts after reporting them, so the next stream's" +
			" summary carries the last one's audio too")
	}
}

func TestReportingAStreamThatCarriedNothingSaysNothing(t *testing.T) {
	peer := &untrustedlog.Log{}
	var run chunkRun
	run.report(peer, "server")
	if peer.Written() != 0 {
		t.Error("a stream that carried no audio still wrote a summary, so every" +
			" stream/start spends a peer line on nothing")
	}
}

func feedChunks(t *testing.T, run *chunkRun, peer *untrustedlog.Log, s *Session, n int) {
	t.Helper()
	for range n {
		c, err := s.AudioChunk(chunkBody(1, make([]byte, 4)))
		if err != nil {
			t.Fatalf("AudioChunk: %v", err)
		}
		run.took(peer, "server", s, c)
	}
}

func TestOnlyTheFirstChunkOfAStreamIsAnnouncedAsTheFirst(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 2)
	run.due = time.Now().Add(-time.Second)
	feedChunks(t, &run, peer, s, 2)

	if got := strings.Count(out.String(), "its first chunk"); got != 1 {
		t.Errorf("one stream announced a first chunk %d times; the summary resets the"+
			" counts, so every summary is followed by a second start that never happened",
			got)
	}
}

func TestAStreamAnnouncesItsFirstChunkEvenWithNoClockYet(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	s.clock = nil
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 1)
	if !strings.Contains(out.String(), "its first chunk") {
		t.Error("audio began arriving before the clock converged and nothing said so," +
			" and no later chunk will say it either")
	}
}

func TestASummaryIsDueOnTimeRatherThanOnACount(t *testing.T) {
	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 2000)
	if run.chunks != 2000 {
		t.Errorf("a summary was written after %d chunks; at 25 ms each a count spends the"+
			" twenty-a-minute peer budget on whatever the server picks for a chunk size",
			2000-run.chunks)
	}
}

func TestADroppedConnectionStillSummarisesWhatItHeard(t *testing.T) {
	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 3)
	before := peer.Written()
	run.done(peer, "server")
	if peer.Written() == before {
		t.Error("a connection that dropped mid-stream threw away what it had counted," +
			" which is exactly the run somebody reads the log to ask about")
	}
	if run.announced {
		t.Error("the next stream on a new connection would not announce its first chunk")
	}
}

func TestALeadAlreadyPastDueReadsDifferentlyFromOneAtZero(t *testing.T) {
	if micros(-400).String() == micros(0).String() {
		t.Error("a chunk 400 us late prints the same as one due now, so a stream that has" +
			" already started arriving late reads as one merely running tight")
	}
	if got := micros(-400).String(); got[0] != '-' {
		t.Errorf("a lead of -400 us printed as %q, which hides that it is in the past", got)
	}
}

func TestClearingKeepsTheStreamItAlreadyAnnounced(t *testing.T) {
	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 1)
	run.report(peer, "server")
	if !run.announced {
		t.Error("a clear left the stream open but forgot it had announced it, so the next" +
			" chunk of the same stream is logged as another stream starting")
	}
}

func TestASummaryCarriesTheClockThatTimedIt(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	s.clock = newClock()
	for i := range 4 {
		s.clock.filter.Update(1000, 50, int64(i)*1000)
	}
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 2)
	run.report(peer, "server")

	if !strings.Contains(out.String(), "against a clock good to") {
		t.Error("the summary says what the leads were and not what clock measured them," +
			" so a lead that moved cannot be told from a clock that did")
	}
}

func TestASummaryReportsHowFarTheClockMovedUnderIt(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	s.clock = newClock()
	for i := range 4 {
		s.clock.filter.Update(1000, 50, int64(i)*1000)
	}
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 1)
	for i := range 6 {
		s.clock.filter.Update(900_000, 50, 10_000+int64(i)*1000)
	}
	feedChunks(t, &run, peer, s, 1)
	run.report(peer, "server")

	if strings.Contains(out.String(), "that moved 0s") {
		t.Error("the clock's offset moved under the stream and the summary reported no" +
			" movement, so a lead anomaly reads as the server's fault either way")
	}
}

func TestTheNextWindowMeasuresTheClockFromWhereTheLastOneLeftIt(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	s.clock = newClock()
	for i := range 6 {
		s.clock.filter.Update(900_000, 50, int64(i)*1000)
	}
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 1)
	run.report(peer, "server")

	out.Reset()
	feedChunks(t, &run, peer, s, 1)
	run.report(peer, "server")

	if !strings.Contains(out.String(), "that moved 0s") {
		t.Errorf("a window over a clock that never moved reported movement: %q; the"+
			" baseline restarted at zero, so every window blames the clock", out.String())
	}
}

func TestEachWindowMeasuresItsOwnLeadBounds(t *testing.T) {
	peer := &untrustedlog.Log{}
	s := held()
	s.clock = newClock()
	for i := range 6 {
		s.clock.filter.Update(1000, 50, int64(i)*1000)
	}
	p := ours()
	startWith(t, s, &p)

	run := chunkRun{every: reportEvery}
	feedChunks(t, &run, peer, s, 2)
	first := run.leastLead
	run.report(peer, "server")

	feedChunks(t, &run, peer, s, 2)
	if run.leastLead == 0 || run.mostLead == 0 {
		t.Errorf("the second window reported bounds of %d and %d; one of them is the"+
			" zero value rather than a lead, because the window never seeded itself",
			run.leastLead, run.mostLead)
	}
	if run.leastLead > run.mostLead {
		t.Errorf("the second window's bounds are inverted: %d to %d",
			run.leastLead, run.mostLead)
	}
	if first == 0 {
		t.Fatal("the first window measured nothing, so the comparison proves nothing")
	}
}

func TestASummarySaysHowMuchOfTheWindowWasSilence(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	peer := &untrustedlog.Log{}
	s := held()
	p := ours()
	startWith(t, s, &p)

	stream := &fakeStream{}
	run := chunkRun{every: reportEvery, play: &playback{stream: stream}}
	feedChunks(t, &run, peer, s, 2)

	stream.heard(StreamRate, StreamRate/2)
	run.report(peer, "server")
	if got := out.String(); !strings.Contains(got, "placed 1s of audio against 500ms of silence") {
		t.Errorf("the summary does not say what the player did with the window, so a gap"+
			" cannot be told from audio that arrived and played:\n%s", got)
	}

	out.Reset()
	feedChunks(t, &run, peer, s, 2)
	stream.heard(2*StreamRate, StreamRate/2)
	run.report(peer, "server")
	if got := out.String(); !strings.Contains(got, "placed 1s of audio against 0s of silence") {
		t.Errorf("the second window reports the whole stream rather than the window, so a"+
			" gap at one track boundary reads as a player that is always short:\n%s", got)
	}
}

func TestASummaryAfterAFreshStreamCountsFromItsOwnZero(t *testing.T) {
	first := &fakeStream{}
	play := playback{stream: first}
	run := chunkRun{every: reportEvery, play: &play}

	first.heard(5*StreamRate, StreamRate/2)
	run.placedSince()

	second := &fakeStream{}
	play.stream = second
	second.heard(30*StreamRate, StreamRate)

	audio, silence := run.placedSince()
	if audio != 30*StreamRate || silence != StreamRate {
		t.Errorf("the first window of a replaced stream reported %d and %d, want its"+
			" own %d and %d. A fresh stream counts from zero, so a baseline carried"+
			" over from the one before it undercounts the window by that whole"+
			" stream -- and it does so silently, because the difference only looks"+
			" wrong once the new stream has outrun the old one",
			audio, silence, 30*StreamRate, StreamRate)
	}
}
