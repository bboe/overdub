package alexa

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type levels struct {
	mu    sync.Mutex
	seen  []int
	music []bool
}

func focusWatcher() (*FocusWatcher, *levels) {
	l := &levels{}
	w := &FocusWatcher{
		alive: func(string) bool { return true },
		OnLevel: func(percent int) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.seen = append(l.seen, percent)
		},
		OnMusic: func(playing bool) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.music = append(l.music, playing)
		},
	}
	return w, l
}

func (l *levels) playing() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.music...)
}

func (l *levels) heard() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.seen...)
}

func (l *levels) last() int {
	seen := l.heard()
	if len(seen) == 0 {
		return fullPercent
	}
	return seen[len(seen)-1]
}

const (
	listenTake   = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:5e] FocusManager:acquireChannel:channelName=Dialog,interface=SpeechRecognizer"
	listenGive   = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:5e] FocusManager:releaseChannel:channelName=Dialog"
	replyTake    = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:5f] FocusManager:acquireChannel:channelName=Dialog,interface=SpeechSynthesizer"
	replyGive    = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:5f] FocusManager:releaseChannel:channelName=Dialog"
	alertTake    = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:69] FocusManager:acquireChannel:channelName=Alert,interface=Alerts"
	alertGive    = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:69] FocusManager:releaseChannel:channelName=Alert"
	callTake     = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:71] FocusManager:acquireChannel:channelName=Communications,interface=SipUserAgent"
	musicTake    = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:62] FocusManager:acquireChannel:channelName=Content,interface=AudioPlayer"
	musicGive    = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:62] FocusManager:releaseChannel:channelName=Content"
	replyPlaying = "11-14 19:04:06.783  1003  2114 I PuffinApp: [e:60] TTSObserverAdapter:onStateChanged::onStateChanged:SpeechSynthesizerState=PLAYING,mediaSourceId=0"
)

func lines(ls ...string) *strings.Reader {
	return strings.NewReader(strings.Join(ls, "\n") + "\n")
}

func TestAReplyDucksUntilItsChannelIsReleased(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(replyTake, replyPlaying))
	if got := l.last(); got != duckPercent {
		t.Fatalf("level %d while she speaks, want %d", got, duckPercent)
	}
	w.read(lines(replyGive))
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d]", got, duckPercent, fullPercent)
	}
}

func TestTheListenersLateReleaseDoesNotEndTheReply(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(listenTake, replyTake, listenGive))
	if got := l.last(); got != duckPercent {
		t.Fatalf("level %d after the recognizer released a channel the synthesizer had"+
			" taken over, want %d: she is still speaking", got, duckPercent)
	}
	w.read(lines(replyGive))
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d]", got, duckPercent, fullPercent)
	}
}

func TestAnAlertDucksDeeperThanHerSpeech(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(alertTake, replyTake))
	if got := l.last(); got != deepDuckPercent {
		t.Fatalf("level %d with an alert and a reply held, want %d: the deepest wins",
			got, deepDuckPercent)
	}
	w.read(lines(alertGive, replyGive))
	if got := l.heard(); !slices.Equal(got, []int{deepDuckPercent, duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d %d]", got, deepDuckPercent, duckPercent,
			fullPercent)
	}
}

func TestACallDucksDeep(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(callTake))
	if got := l.last(); got != deepDuckPercent {
		t.Errorf("level %d during a call, want %d", got, deepDuckPercent)
	}
}

func TestHerOwnMusicDoesNotDuck(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(musicTake, musicGive))
	if got := l.heard(); len(got) != 0 {
		t.Errorf("heard levels %v for her music, want none: it plays alongside", got)
	}
}

func TestHerSpeechOverHerMusicDucks(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(musicTake, replyTake, replyGive, musicGive))
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d]: her reply ducks whatever else holds a"+
			" channel", got, duckPercent, fullPercent)
	}
}

func TestHerMusicIsReportedBothWays(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(musicTake))
	if got := l.playing(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("heard music %v once she took the content channel, want [true]", got)
	}
	w.read(lines(replyTake, replyGive, musicGive))
	if got := l.playing(); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("heard music %v, want [true false]: her reply is not her music", got)
	}
}

func TestHerMusicNeverExpires(t *testing.T) {
	w, l := focusWatcher()
	now := time.Now()
	w.now = func() time.Time { return now }

	w.read(lines(musicTake))
	now = now.Add(4 * holdFor)
	w.expire()
	if got := l.playing(); !slices.Equal(got, []bool{true}) {
		t.Errorf("heard music %v after %v, want [true]: her music runs for hours, and an"+
			" expiry would hand the speaker back mid-song", got, 4*holdFor)
	}
}

func TestOtherChannelsThatDoNotDuckAreNotHeld(t *testing.T) {
	w, _ := focusWatcher()

	w.read(lines("11-14 19:04:06.783  1003  2114 I PuffinApp: [e:70] FocusManager:acquireChannel:channelName=Visual,interface=TemplateRuntime"))
	if len(w.holders) != 0 {
		t.Errorf("holding %v for a channel that neither ducks nor is her music", w.holders)
	}
}

func TestAPuffinAppRestartReleasesEverything(t *testing.T) {
	w, l := focusWatcher()
	w.alive = func(string) bool { return false }

	w.read(lines(musicTake, replyTake,
		"11-14 19:04:06.783  2044  2114 I PuffinApp: [e:12] AlexaClientSDKInit:initialize"))
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d]: the process that took the channel is gone"+
			" and will never release it", got, duckPercent, fullPercent)
	}
	if got := l.playing(); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("heard music %v, want [true false]", got)
	}

	w.read(lines("11-14 19:04:06.783  2044  2114 I PuffinApp: [e:63] FocusManager:acquireChannel:channelName=Content,interface=AudioPlayer"))
	if got := l.playing(); !slices.Equal(got, []bool{true, false, true}) {
		t.Errorf("heard music %v after the new process took the channel, want [true false true]", got)
	}
}

func TestAReleaseWithMoreFieldsStillReleases(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(replyTake, replyGive+",interface=SpeechSynthesizer"))
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d]: a release cut differently from its acquire"+
			" never matches, and every reply ducks for %v", got, duckPercent, fullPercent, holdFor)
	}
}

func TestTheFollowerStartsAtTheMomentItIsStarted(t *testing.T) {
	at := time.Unix(1794700000, 123456789)
	argv := strings.Join(focusArgv(at), " ")
	if !strings.Contains(argv, "-T 1794700000.123 ") {
		t.Errorf("logcat runs as %q, want it to start at 1794700000.123: -T 1 replays the"+
			" newest line, and a channel taken in it may have been released while logcat"+
			" was down", argv)
	}
	if !strings.Contains(argv, "-v threadtime") {
		t.Errorf("logcat runs as %q, want threadtime: it prints the process id apart from"+
			" the thread's", argv)
	}
}

func TestTheLevelIsOnlySentWhenItChanges(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(listenTake, replyTake, replyTake, listenGive, replyGive, replyGive, alertGive))
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, fullPercent}) {
		t.Errorf("heard levels %v, want [%d %d]: a level already set is not news",
			got, duckPercent, fullPercent)
	}
}

func TestAHoldNobodyReleasesExpires(t *testing.T) {
	w, l := focusWatcher()
	now := time.Now()
	w.now = func() time.Time { return now }

	w.read(lines(replyTake))
	now = now.Add(holdFor - time.Minute)
	w.read(lines(alertTake))

	now = now.Add(2 * time.Minute)
	w.expire()
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, deepDuckPercent}) {
		t.Fatalf("heard levels %v after the reply's hold expired, want [%d %d]: the alert"+
			" was taken later and has not", got, duckPercent, deepDuckPercent)
	}

	now = now.Add(holdFor)
	w.expire()
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, deepDuckPercent, fullPercent}) {
		t.Fatalf("heard levels %v, want [%d %d %d]: a missed release would otherwise leave"+
			" the music ducked for good", got, duckPercent, deepDuckPercent, fullPercent)
	}

	w.expire()
	if got := l.heard(); len(got) != 3 {
		t.Errorf("heard levels %v; a sweep with nothing to expire sent a level", got)
	}
}

func TestALogcatThatEndsLeavesTheMusicAtFullLevel(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(replyTake, alertTake, musicTake))
	w.reset()
	w.reset()
	if got := l.playing(); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("heard music %v, want [true false]", got)
	}
	if got := l.heard(); !slices.Equal(got, []int{duckPercent, deepDuckPercent, fullPercent}) {
		t.Fatalf("heard levels %v, want [%d %d %d]: the releases logcat missed while it"+
			" was down will never arrive", got, duckPercent, deepDuckPercent, fullPercent)
	}

	w.read(lines(replyGive))
	if got := l.heard(); len(got) != 3 {
		t.Errorf("heard levels %v; a holder survived the reset", got)
	}
}

func TestAResetAtFullLevelSendsNothing(t *testing.T) {
	w, l := focusWatcher()

	w.reset()
	if got := l.heard(); len(got) != 0 {
		t.Errorf("heard levels %v from a reset with nothing held, want none", got)
	}
}

func TestLinesThatAreNotFocusChangesAreIgnored(t *testing.T) {
	w, l := focusWatcher()

	w.read(lines(
		"",
		"--------- beginning of main",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: FocusManager:acquireChannel:channelName=Dialog,interface=SpeechSynthesizer",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: [] FocusManager:acquireChannel:channelName=Dialog,interface=SpeechSynthesizer",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: e:5f] FocusManager:acquireChannel:channelName=Dialog,interface=SpeechSynthesizer",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: [e:5f] FocusManager:acquireChannel:channelName=,interface=SpeechSynthesizer",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: [e:5f] FocusManager:acquireChannel:channelName=",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: [e:4c] SpeechMediaPlayer:onFocusChanged:newFocus=FOREGROUND,MixingBehavior=PRIMARY",
		"11-14 19:04:06.783  1003  2114 I PuffinApp: [e:69] AlertsCapabilityAgent:acquireChannel:token=amzn1.as-ct.v1",
		replyPlaying,
	))
	if got := l.heard(); len(got) != 0 {
		t.Errorf("heard levels %v from lines that take no channel, want none", got)
	}
}

func TestAPidChangeWhileTheFirstProcessRunsIsNotARestart(t *testing.T) {
	w, l := focusWatcher()
	live := map[string]bool{"1074": true, "1562": true}
	w.alive = func(pid string) bool { return live[pid] }

	w.read(lines("11-14 19:04:06.783  1074  2114 I PuffinApp: [e:63] FocusManager:acquireChannel:channelName=Content,interface=AudioPlayer"))
	w.read(lines("11-14 19:04:07.000  1562  1562 I PuffinApp: [e:af] CurlSocketMonitor:ConnectionDump"))
	live["1562"] = false
	w.read(lines("11-14 19:04:08.000  1074  2114 I PuffinApp: [e:5f] FocusManager:acquireChannel:channelName=Dialog,interface=SpeechSynthesizer"))
	if got := l.playing(); !slices.Equal(got, []bool{true}) {
		t.Errorf("heard music %v, want [true]: the process that took the channel never"+
			" stopped, whatever else logged in between", got)
	}
	if got := l.heard(); !slices.Equal(got, []int{duckPercent}) {
		t.Errorf("heard levels %v, want [%d]", got, duckPercent)
	}
}

func TestAPuffinAppThatDiesSilentlyIsReleasedByTheSweep(t *testing.T) {
	w, l := focusWatcher()
	live := true
	w.alive = func(string) bool { return live }

	w.read(lines(musicTake))
	w.expire()
	if got := l.playing(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("heard music %v while PuffinApp runs, want [true]", got)
	}
	live = false
	w.expire()
	if got := l.playing(); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("heard music %v after PuffinApp died, want [true false]: no line will ever"+
			" release her channel, and sendspin would stay unavailable", got)
	}
	live = true
	w.read(lines("11-14 19:05:00.000  2044  2044 I PuffinApp: [e:63] FocusManager:acquireChannel:channelName=Content,interface=AudioPlayer"))
	if got := l.playing(); !slices.Equal(got, []bool{true, false, true}) {
		t.Errorf("heard music %v once the new PuffinApp took the channel, want [true false true]", got)
	}
}
