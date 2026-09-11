package alexa

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type heard struct {
	mu      sync.Mutex
	starts  int
	ends    int
	lastOK  bool
	details []string
}

func watcher() (*PlaybackWatcher, *heard) {
	h := &heard{}
	w := &PlaybackWatcher{
		OnStart: func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.starts++
		},
		OnEnd: func(ok bool, detail string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.ends++
			h.lastOK = ok
			h.details = append(h.details, detail)
		},
	}
	return w, h
}

func (h *heard) state() (int, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts, h.ends, h.lastOK
}

const (
	startLine = "I/tts-Server(  940): Playback started: uid(32037)_id(0)_namespace(SpeechSynthesizer)"
	endOK     = "I/tts-Server(  940): Playback ended: uid(32037)_id(0)_namespace(SpeechSynthesizer), SUCCESS"
	endFailed = "I/tts-Server(  940): Playback ended: uid(32037)_id(0)_namespace(SpeechSynthesizer), FAILED"
)

func TestAPlaybackWeAskedForIsReportedBothWays(t *testing.T) {
	w, h := watcher()
	w.Expect(time.Minute)
	w.read(strings.NewReader(startLine + "\n" + endOK + "\n"))

	if starts, ends, ok := h.state(); starts != 1 || ends != 1 || !ok {
		t.Errorf("heard %d starts and %d ends, ok=%v; want one of each and a success",
			starts, ends, ok)
	}
}

func TestAPlaybackNobodyAskedForIsNotOurs(t *testing.T) {
	w, h := watcher()
	w.read(strings.NewReader(startLine + "\n" + endOK + "\n"))

	if starts, ends, _ := h.state(); starts != 0 || ends != 0 {
		t.Errorf("heard %d starts and %d ends without expecting any: Alexa speaks for her "+
			"own reasons all day and none of them are our clip", starts, ends)
	}
}

func TestTheWatcherDisarmsOnTheEndItWasWaitingFor(t *testing.T) {
	w, h := watcher()
	w.Expect(time.Minute)
	w.read(strings.NewReader(startLine + "\n" + endOK + "\n" + startLine + "\n" + endOK + "\n"))

	if starts, ends, _ := h.state(); starts != 1 || ends != 1 {
		t.Errorf("heard %d starts and %d ends, want one of each: the second playback is hers",
			starts, ends)
	}
}

func TestTheDecoderComplaintTravelsWithTheFailure(t *testing.T) {
	w, h := watcher()
	w.Expect(time.Minute)
	w.read(strings.NewReader(
		"E/tts-Server( 12): cannot estimate length of the next mp3 frame\n" + endFailed + "\n"))

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ends != 1 || h.lastOK {
		t.Fatalf("heard %d ends, ok=%v; want one failure", h.ends, h.lastOK)
	}
	if len(h.details) != 1 || !strings.Contains(h.details[0], "cannot estimate length") {
		t.Errorf("the failure carried %q, want the decoder's own complaint: a wrong encoding "+
			"and a missing file both end FAILED and only this separates them", h.details)
	}
}

func TestAnotherAgentsPlaybackIsNotOurs(t *testing.T) {
	w, h := watcher()
	w.Expect(time.Minute)
	w.read(strings.NewReader(
		"I/tts-Server(  940): Playback started: uid(1)_id(0)_namespace(AudioPlayer)\n" +
			"I/tts-Server(  940): Playback ended: uid(1)_id(0)_namespace(AudioPlayer), SUCCESS\n"))

	if starts, ends, _ := h.state(); starts != 0 || ends != 0 {
		t.Errorf("heard %d starts and %d ends for AudioPlayer, which is music rather than "+
			"speech and never ours", starts, ends)
	}
}

func TestCancelStopsUsClaimingTheNextPlayback(t *testing.T) {
	w, h := watcher()
	w.Expect(time.Minute)
	w.Cancel()
	w.read(strings.NewReader(startLine + "\n" + endOK + "\n"))

	if starts, ends, _ := h.state(); starts != 0 || ends != 0 {
		t.Errorf("heard %d starts and %d ends after a cancel", starts, ends)
	}
}

func TestAPlaybackThatNeverReportsEndsAnyway(t *testing.T) {
	w, h := watcher()
	now := time.Now()
	w.now = func() time.Time { return now }
	w.Expect(time.Minute)

	w.expire()
	if _, ends, _ := h.state(); ends != 0 {
		t.Fatal("the wait ended before its deadline")
	}

	now = now.Add(2 * time.Minute)
	w.expire()
	if _, ends, ok := h.state(); ends != 1 || ok {
		t.Errorf("heard %d ends, ok=%v; want one failure: a clip Alexa never mentions would "+
			"otherwise leave the player playing for ever", ends, ok)
	}

	w.expire()
	if _, ends, _ := h.state(); ends != 1 {
		t.Errorf("the timeout fired %d times, want once", ends)
	}
}

func TestTheFixturesAreTheLinesADotPrints(t *testing.T) {
	for _, line := range []string{startLine, endOK, endFailed} {
		if !strings.Contains(line, "namespace(SpeechSynthesizer)") {
			t.Errorf("fixture %q does not carry the namespace the way the device does", line)
		}
		if strings.Contains(line, "dir") {
			t.Errorf("fixture %q carries a directive id, and the device's lines do not: "+
				"a watcher written against an invented line matches nothing real", line)
		}
	}
}

func TestALogcatThatWillNotStartIsSaidOnce(t *testing.T) {
	boom := errors.New("fork/exec logcat: no such file or directory")

	say, said := shouldSay(false, 0, boom)
	if !say {
		t.Fatal("the first failure was not reported at all")
	}
	for i := 0; i < 100; i++ {
		say, said = shouldSay(said, 0, boom)
		if say {
			t.Fatalf("failure %d was reported again; at %v apiece that is a line a minute "+
				"for ever, into a log on /data that is truncated at boot", i+2, watchRetry)
		}
	}

	if say, _ = shouldSay(said, watchRetry*3, boom); !say {
		t.Error("a tail that ran for a while and then failed said nothing: that is a new " +
			"fault rather than the one already reported")
	}
	if say, _ = shouldSay(false, 0, nil); say {
		t.Error("a tail that ended without error was reported as a failure")
	}
}

func TestAClipLongerThanTheStartDeadlineIsNotAFailure(t *testing.T) {
	w, h := watcher()
	now := time.Now()
	w.now = func() time.Time { return now }
	w.Expect(30 * time.Second)

	w.read(strings.NewReader(startLine + "\n"))
	if starts, _, _ := h.state(); starts != 1 {
		t.Fatal("the start was not reported")
	}

	now = now.Add(2 * time.Minute)
	w.expire()
	if _, ends, _ := h.state(); ends != 0 {
		t.Fatalf("a clip still playing after %v was reported as a failure: the deadline is "+
			"there to catch a clip she never starts, and a long clip is not that", 2*time.Minute)
	}

	w.read(strings.NewReader(endOK + "\n"))
	if _, ends, ok := h.state(); ends != 1 || !ok {
		t.Errorf("the real end reported %d ends ok=%v, want one success", ends, ok)
	}
}

func TestAPlaybackThatStartsAndNeverEndsStillGivesUp(t *testing.T) {
	w, h := watcher()
	now := time.Now()
	w.now = func() time.Time { return now }
	w.Expect(30 * time.Second)
	w.read(strings.NewReader(startLine + "\n"))

	now = now.Add(playingFor + time.Minute)
	w.expire()
	if _, ends, ok := h.state(); ends != 1 || ok {
		t.Errorf("a playback that started and never ended reported %d ends ok=%v; want one "+
			"failure, or the entity plays for the rest of the run", ends, ok)
	}
}

func TestExtendDoesNotReviveAFinishedPlayback(t *testing.T) {
	w, h := watcher()
	now := time.Now()
	w.now = func() time.Time { return now }
	w.Expect(30 * time.Second)

	w.read(strings.NewReader(endFailed + "\n"))
	if _, ends, _ := h.state(); ends != 1 {
		t.Fatal("the failure was not reported")
	}

	w.Extend(30 * time.Second)
	now = now.Add(2 * time.Minute)
	w.expire()
	if _, ends, _ := h.state(); ends != 1 {
		t.Errorf("the watch reported %d ends; a clip that already failed was given a fresh "+
			"deadline and then failed a second time for nobody", ends)
	}
}

func TestExtendKeepsTheReasonAlreadyHeard(t *testing.T) {
	w, h := watcher()
	w.Expect(time.Minute)
	w.read(strings.NewReader("E/tts-Server( 12): cannot estimate length of the next mp3 frame\n"))
	w.Extend(time.Minute)
	w.read(strings.NewReader(endFailed + "\n"))

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.details) != 1 || !strings.Contains(h.details[0], "cannot estimate length") {
		t.Errorf("the failure carried %q; extending a watch threw away the line that says why",
			h.details)
	}
}
