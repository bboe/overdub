package device

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Captured from the Dot with dumpsys media.audio_flinger, the lines reduced to
// the ones the parser walks but each of them verbatim. The input thread is the
// microphone and is always active, which is why counting tracks without knowing
// which thread they are under would report the Dot as playing for ever. The
// numTracks= and Fast tracks: lines are the near misses the pattern has to
// decline, and both are real.
const flingerDump = `Output thread 0xf5b6e008:
            numTracks=1 writeErrors=0 underruns=4 overruns=8
  Fast tracks: kMaxFastTracks=8 activeMask=0x1
  1 Tracks of which 1 are active
Input thread 0xf5136008:
  1 Tracks of which 1 are active
Reroute submix audio module:
`

// The same device idle. It writes the short form here, without the "of which"
// clause, which is the form the Dot actually produces when nothing is playing.
const flingerIdle = `Output thread 0xf5b6e008:
  0 Tracks
Input thread 0xf5136008:
  1 Tracks of which 1 are active
Reroute submix audio module:
`

func TestHasOutputTrack(t *testing.T) {
	for _, tt := range []struct {
		name       string
		dump       string
		playing    bool
		recognised bool
	}{
		{"an active output track", flingerDump, true, true},
		{"no output track", flingerIdle, false, true},
		// The input thread is the microphone. Counting its tracks would report
		// the Dot as playing whenever it is listening, which is always.
		{"an input track is not playback",
			"Output thread 0x1:\n  0 Tracks of which 0 are active\n" +
				"Input thread 0x2:\n  3 Tracks of which 3 are active\n", false, true},
		// Tracks that exist but are not active is what a paused stream leaves
		// behind, and it is the count after "of which" that decides.
		{"tracks present but none active",
			"Output thread 0x1:\n  2 Tracks of which 0 are active\n", false, true},
		// The older form has no "of which" clause at all, so the first count is
		// the only one there is.
		{"the short form without an active count",
			"Output thread 0x1:\n  1 Tracks\n", true, true},
		{"the short form with no tracks",
			"Output thread 0x1:\n  0 Tracks\n", false, true},
		// A dump that names no output thread is one this parser does not
		// understand, which is a different answer from "nothing is playing".
		{"no output thread at all", "Input thread 0x2:\n  1 Tracks of which 1 are active\n", false, false},
		// An output thread whose track lines no longer parse is the shape an
		// AudioFlinger format change takes, and it must not read as silence.
		{"an output thread whose track lines do not parse",
			"Output thread 0x1:\n  Tracks: 1 (1 active)\n  Standby: no\n", false, false},
		// Every output thread has to be accounted for. One that parsed does
		// not answer for one that did not, in either order.
		// The playing answer has to be accounted for too. A thread that says
		// something is playing does not excuse one whose lines did not parse:
		// answering yes there and no reading when idle gives an entity that
		// goes on and unknown and never off.
		{"an active thread beside one that does not parse",
			"Output thread 0x1:\n  1 Tracks of which 1 are active\n" +
				"Output thread 0x2:\n  Tracks: 1 (1 active)\n", false, false},
		{"an unparsed thread before an active one",
			"Output thread 0x1:\n  Tracks: 1 (1 active)\n" +
				"Output thread 0x2:\n  1 Tracks of which 1 are active\n", false, false},
		{"a second output thread that does not parse",
			"Output thread 0x1:\n  0 Tracks of which 0 are active\n" +
				"Output thread 0x2:\n  Tracks: 1 (1 active)\n", false, false},
		{"an unparsed output thread before one that parses",
			"Output thread 0x1:\n  Tracks: 1 (1 active)\n" +
				"Output thread 0x2:\n  0 Tracks of which 0 are active\n", false, false},
		// The left margin is what ends the thread in a real dump: this one
		// never ends on an output thread, so the section below it is what has
		// to notice that its counts were never read. Every other unparsed case
		// here is caught by the thread after it or by the end of the dump.
		{"an unparsed output thread before a later section",
			"Output thread 0x1:\n  Tracks: 1 (1 active)\n  Standby: no\n" +
				"Input thread 0x2:\n  1 Tracks of which 1 are active\n" +
				"Reroute submix audio module:\n", false, false},
		// Two output threads that both parse, one active. AudioFlinger runs a
		// primary, a deep buffer and a fast mixer, so this is the ordinary
		// shape rather than a corner, and the answer is any thread rather than
		// the last one.
		{"an active output thread beside an idle one",
			"Output thread 0x1:\n  1 Tracks of which 1 are active\n" +
				"Output thread 0x2:\n  0 Tracks of which 0 are active\n", true, true},
		{"an idle output thread beside an active one",
			"Output thread 0x1:\n  0 Tracks of which 0 are active\n" +
				"Output thread 0x2:\n  1 Tracks of which 1 are active\n", true, true},
		// A blank line and a tab-indented line are both inside the thread, not
		// a new section at the left margin. Either one ending the thread would
		// leave its count unread and turn the whole dump into no reading.
		{"a blank line does not end the thread",
			"Output thread 0x1:\n\n  1 Tracks of which 1 are active\n", true, true},
		{"a tab-indented line does not end the thread",
			"Output thread 0x1:\n\tStandby: no\n  1 Tracks of which 1 are active\n", true, true},
		// The count has to be the first thing on its line. Without that the
		// stream's own name carries a number of tracks that is not the
		// thread's, and the thread's real count is then the second reading.
		{"a track count that is not at the start of its line",
			"Output thread 0x1:\n  AudioStreamOut: 7 Tracks of which 7 are active\n" +
				"  0 Tracks of which 0 are active\n", false, true},
		// A section at the left margin ends the thread above it, so its
		// counts are not the thread's.
		{"a track count in a later section is not the thread's",
			"Output thread 0x1:\n  0 Tracks of which 0 are active\n" +
				"Global session refs:\n  4 Tracks of which 4 are active\n", false, true},
		{"nothing at all", "", false, false},
		{"not the dump we expected", "Permission denied\n", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			playing, recognised := hasOutputTrack([]byte(tt.dump))
			if playing != tt.playing || recognised != tt.recognised {
				t.Errorf("hasOutputTrack = (%v, %v), want (%v, %v)",
					playing, recognised, tt.playing, tt.recognised)
			}
		})
	}
}

// The glob is resolved once and the paths kept: it costs 5.6ms against the
// 2.0ms of reading every file it finds, and substreams do not appear or move
// while the kernel is up. docs/architecture.md carries the measurement.
func TestThePCMPathsAreResolvedOnce(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()

	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(sub, "status")
	if err := os.WriteFile(status, []byte("closed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The states a substream passes through on the way in and out are not
	// RUNNING and are not playback, and only RUNNING is. Testing the match
	// against "closed" alone would let a prefix match through.
	for _, state := range []string{"closed\n", "state: SETUP\n", "state: PREPARED\n",
		"state: DRAINING\n", "state: XRUN\n", "state: PAUSED\n"} {
		if err := os.WriteFile(status, []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
		if running, ok := pcmRunning(); running || !ok {
			t.Errorf("a substream in %q reads as (%v, %v), want (false, true)",
				strings.TrimSpace(state), running, ok)
		}
	}
	if err := os.WriteFile(status, []byte("closed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := len(pcmStatusPaths()); got != 1 {
		t.Fatalf("resolved %d paths, want 1", got)
	}

	if err := os.WriteFile(status, []byte("state: RUNNING\nowner_pid: 123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if running, ok := pcmRunning(); !running || !ok {
		t.Error("a RUNNING substream does not read as running, so the cached path is not being re-read")
	}

	// A substream that appears later is not picked up, because the paths are
	// not globbed again. That is the trade the caching makes and it is only
	// safe because these do not appear after boot.
	late := filepath.Join(dir, "card0", "pcm9p", "sub0")
	if err := os.MkdirAll(late, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(late, "status"), []byte("state: RUNNING\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := len(pcmStatusPaths()); got != 1 {
		t.Errorf("resolved %d paths after one appeared, want the 1 already cached", got)
	}
}

// Nothing matching is the early-boot case rather than a permanent answer, so it
// is retried rather than cached as empty.
func TestAnEmptyGlobIsRetried(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()

	// Nothing matching is not silence, it is early boot: no reading rather
	// than a confident off.
	if running, ok := pcmRunning(); running || ok {
		t.Errorf("an empty glob reads as (%v, %v), want (false, false)", running, ok)
	}

	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "status"), []byte("state: RUNNING\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if running, _ := pcmRunning(); !running {
		t.Error("a substream that appeared after an empty glob is never found; the empty result was cached")
	}
}

// A dumpsys that cannot be read is no reading rather than a plausible one.
// Reporting "playing" there is what target did, and it publishes a value Home
// Assistant draws as a measurement at the moment we know least.
func TestAnUnreadableDumpIsNoReading(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()
	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "status"), []byte("state: RUNNING\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name    string
		out     []byte
		err     error
		playing bool
		ok      bool
	}{
		{"the command failed", nil, errors.New("no such file"), false, false},
		{"the dump named no output thread", []byte("Permission denied\n"), nil, false, false},
		{"an active track", []byte(flingerDump), nil, true, true},
		{"no active track", []byte(flingerIdle), nil, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := speakerCommand
			speakerCommand = func(context.Context) ([]byte, error) { return tt.out, tt.err }
			defer func() { speakerCommand = old }()

			playing, ok := SpeakerPlaying()
			if playing != tt.playing || ok != tt.ok {
				t.Errorf("SpeakerPlaying = (%v, %v), want (%v, %v)", playing, ok, tt.playing, tt.ok)
			}
		})
	}
}

// A substream that is not running is an answer on its own, and the expensive
// half is skipped for it: the fork is what this reading costs, and it is only
// worth paying when something might be coming out.
func TestASilentSubstreamNeverForks(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()
	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "status"), []byte("closed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	forked := false
	old := speakerCommand
	speakerCommand = func(context.Context) ([]byte, error) {
		forked = true
		return []byte(flingerDump), nil
	}
	defer func() { speakerCommand = old }()

	playing, ok := SpeakerPlaying()
	if playing || !ok {
		t.Errorf("SpeakerPlaying = (%v, %v), want (false, true)", playing, ok)
	}
	if forked {
		t.Error("a closed substream still forked dumpsys, which is the whole cost of this reading")
	}
}

// The read has to fit inside the interval that takes it, for the reason the
// volume read does: the poll is serial. This one is tighter because it rides
// every second tick rather than every fifth.
// Every test that touches the refresh shrinks it, so the value that ships is
// observed by none of them: a minute and an hour both pass. Both claims resting
// on it are bounded here -- the cost of a glob at that rate, and how long a
// partial set can survive.
func TestThePCMRefreshIsTheOneThatShips(t *testing.T) {
	speakerMu.Lock()
	got := pcmRefresh
	speakerMu.Unlock()
	// A glob costs 5.6ms. Below this it starts to matter against readings that
	// cost 2.0ms twice a second.
	if got < 30*time.Second {
		t.Errorf("pcmRefresh is %v, which globs oftener than the reading it guards is worth", got)
	}
	// A set resolved before ALSA finished registering is wrong until this
	// elapses, and it reports confident silence for all of it.
	if got > 5*time.Minute {
		t.Errorf("pcmRefresh is %v, so a partial set survives that long reporting silence", got)
	}
}

func TestTheSpeakerReadBudgetIsBounded(t *testing.T) {
	if got := SpeakerReadBudget(); got != speakerReadTimeout+speakerWaitDelay {
		t.Errorf("SpeakerReadBudget is %v, want the deadline plus the wait delay (%v)",
			got, speakerReadTimeout+speakerWaitDelay)
	}
	// Measured on the Dot at 18.7ms. A budget near that leaves nothing for a
	// loaded device; one near the interval leaves nothing for the poll. The
	// interval itself is main_test.go's to hold, since this package does not
	// know the tick.
	if SpeakerReadBudget() >= 500*time.Millisecond {
		t.Errorf("SpeakerReadBudget is %v, which does not fit inside the half second it is "+
			"read on", SpeakerReadBudget())
	}
	if SpeakerReadBudget() <= 100*time.Millisecond {
		t.Errorf("SpeakerReadBudget is %v against a read measured at 18.7ms, which is too little "+
			"headroom on a loaded device", SpeakerReadBudget())
	}
}

// A set resolved while ALSA is still registering is readable and non-empty, so
// neither the empty-result retry nor the vanished-path one reconsiders it. Left
// alone it would report confident silence for the rest of the boot if pcm23p
// were not in it, which is the mistake the unreadable paths reject, in its
// permanent form.
func TestAPartialSetIsReconsidered(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()
	speakerMu.Lock()
	pcmRefresh = 50 * time.Millisecond
	speakerMu.Unlock()

	write := func(name, body string) {
		t.Helper()
		sub := filepath.Join(dir, "card0", name, "sub0")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "status"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Only the substream that never plays has registered yet.
	write("pcm0p", "closed\n")
	if running, ok := pcmRunning(); running || !ok {
		t.Fatalf("the early set reads as (%v, %v), want (false, true)", running, ok)
	}

	// The one that does appears afterwards, already playing.
	write("pcm23p", "state: RUNNING\n")
	if running, _ := pcmRunning(); running {
		t.Error("the new substream was found before the set was due to be reconsidered")
	}

	time.Sleep(pcmRefresh + 20*time.Millisecond)
	if running, ok := pcmRunning(); !running || !ok {
		t.Errorf("after the refresh interval pcmRunning is (%v, %v), want (true, true): the "+
			"partial set was kept for good", running, ok)
	}
}

// A path that has gone means the cached set is stale, and only an empty set is
// re-globbed. Without invalidating it here one substream disappearing would
// wedge the reading at no-reading for the rest of the boot.
func TestAVanishedPathIsReglobbed(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()

	for _, name := range []string{"pcm23p", "pcm9p"} {
		sub := filepath.Join(dir, "card0", name, "sub0")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "status"), []byte("closed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(pcmStatusPaths()); got != 2 {
		t.Fatalf("cached %d paths, want 2", got)
	}

	if err := os.RemoveAll(filepath.Join(dir, "card0", "pcm9p")); err != nil {
		t.Fatal(err)
	}
	if _, ok := pcmRunning(); ok {
		t.Error("a vanished path still read as a reading")
	}
	// The set is re-globbed, so the one that is left answers from here on.
	if running, ok := pcmRunning(); running || !ok {
		t.Errorf("after the vanished path, pcmRunning is (%v, %v), want (false, true): the "+
			"stale set was never dropped", running, ok)
	}
}

// A status file that cannot be read is no reading either. Reporting silence
// there is the same mistake as reporting playback for a dumpsys that failed:
// both are values Home Assistant draws as a measurement.
func TestAnUnreadableSubstreamIsNoReading(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()

	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(sub, "status")
	if err := os.WriteFile(status, []byte("closed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := pcmRunning(); !ok {
		t.Fatal("a readable substream is not a reading")
	}
	if err := os.Chmod(status, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(status, 0o644)
	if os.Geteuid() == 0 {
		t.Skip("running as root, which reads a 0000 file anyway")
	}

	running, ok := pcmRunning()
	if running || ok {
		t.Errorf("an unreadable substream reads as (%v, %v), want (false, false)", running, ok)
	}
	playing, ok := SpeakerPlaying()
	if playing || ok {
		t.Errorf("SpeakerPlaying on an unreadable substream is (%v, %v), want (false, false)",
			playing, ok)
	}
}

func stubPCM(t *testing.T, glob string) func() {
	t.Helper()
	speakerMu.Lock()
	oldGlob, oldPaths, oldAt, oldRefresh := speakerPCMGlob, pcmPaths, pcmGlobbed, pcmRefresh
	speakerPCMGlob, pcmPaths, pcmGlobbed = glob, nil, time.Time{}
	speakerMu.Unlock()
	return func() {
		speakerMu.Lock()
		speakerPCMGlob, pcmPaths, pcmGlobbed, pcmRefresh = oldGlob, oldPaths, oldAt, oldRefresh
		speakerMu.Unlock()
	}
}

// The budget above is arithmetic over the two numbers, and arithmetic cannot
// show that either of them bounds anything: the deadline, the WaitDelay and the
// argv are all reached through exec, and every other test here replaces
// speakerCommand wholesale. This drives the real one against a child of its own
// choosing, the way facts_test.go does for the volume. The speaker read is the
// tighter of the two, because it rides a tick five times shorter.
func TestOneSpeakerReadCannotOutlastItsBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}

	// The dump is only read when a substream is already running, so without one
	// the fork this test is about is never reached.
	dir := t.TempDir()
	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "status"), []byte("state: RUNNING\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer stubPCM(t, filepath.Join(dir, "card*", "pcm*p", "sub*", "status"))()
	wasArgv, wasTimeout, wasDelay := speakerArgv, speakerReadTimeout, speakerWaitDelay
	defer func() {
		speakerArgv, speakerReadTimeout, speakerWaitDelay = wasArgv, wasTimeout, wasDelay
	}()

	// Scaled down so the test costs half a second rather than the shipped four
	// tenths, and weighted towards the wait: a budget that left it out would be
	// a fifth of this one, which the assertion below can tell from the whole.
	speakerReadTimeout, speakerWaitDelay = 100*time.Millisecond, 400*time.Millisecond
	// The grandchild outlives the kill and holds the inherited stdout open.
	speakerArgv = []string{"sh", "-c", "sleep 30 & sleep 30"}

	start := time.Now()
	playing, ok := SpeakerPlaying()
	elapsed := time.Since(start)

	if ok {
		t.Errorf("a read that never answered reported playing=%v as a reading", playing)
	}
	// A lower bound as well as an upper one. Without it the test passes when the
	// command never ran at all -- a wrong argv fails instantly on any machine
	// with no /system/bin, and an instant failure satisfies every assertion
	// below about a call that finishes inside its budget.
	if elapsed < speakerReadTimeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	// Slack enough for a loaded machine and not enough to hide the deadline
	// standing in for the whole budget: a correct read lands on the budget, and
	// one measured against the deadline alone is five times over it.
	if limit := SpeakerReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, SpeakerReadBudget())
	}
}

// The argv is the one thing the budget test above cannot hold, because it
// replaces it. Two dumpsys reads live in this package and name different
// services: media.audio_flinger carries the track counts, and dumpsys audio is
// the volume's and carries none. A swap parses as a dump this does not
// understand, so the entity reports no reading and goes on doing it, which is
// the failure that says nothing in a log.
func TestTheSpeakerReadsTheFlingerService(t *testing.T) {
	want := []string{"/system/bin/dumpsys", "media.audio_flinger"}
	if len(speakerArgv) != len(want) {
		t.Fatalf("speakerArgv is %v, want %v", speakerArgv, want)
	}
	for i := range want {
		if speakerArgv[i] != want[i] {
			t.Errorf("speakerArgv is %v, want %v", speakerArgv, want)
			break
		}
	}
}

// The other half of the argv pin, and the same argument: every test here
// replaces the glob through stubPCM, so its shipped value is reached by nothing.
// It is the input side of the whole reading. Watching pcm*c rather than pcm*p
// would follow the microphone, whose substream on this Dot is always running,
// so the cheap gate would stop gating and the dump would be forked for every
// tick; reading any file but status would find no state at all.
func TestThePCMGlobWatchesPlaybackSubstreams(t *testing.T) {
	const want = "/proc/asound/card*/pcm*p/sub*/status"
	speakerMu.Lock()
	got := speakerPCMGlob
	speakerMu.Unlock()
	if got != want {
		t.Errorf("speakerPCMGlob is %q, want %q", got, want)
	}
}

// The one pair of substreams the table misses. A file that will not open makes
// the set stale and is otherwise no reading, but a substream already found
// running is an answer, and an answer outranks a gap: the alternative reports
// silence on a Dot that is audibly playing.
func TestARunningSubstreamOutranksAnUnreadableOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which reads a 0000 file anyway")
	}
	dir := t.TempDir()
	defer stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))()

	// pcm0p sorts first, so the unreadable one is met before the running one.
	for _, tt := range []struct {
		pcm, body string
		mode      os.FileMode
	}{
		{"pcm0p", "closed\n", 0o000},
		{"pcm23p", "state: RUNNING\n", 0o644},
	} {
		sub := filepath.Join(dir, "card0", tt.pcm, "sub0")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		status := filepath.Join(sub, "status")
		if err := os.WriteFile(status, []byte(tt.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(status, tt.mode); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(status, 0o644)
	}

	running, ok := pcmRunning()
	if !running || !ok {
		t.Errorf("a running substream beside an unreadable one reads as (%v, %v), want (true, true)",
			running, ok)
	}
}

// The paths are package state behind a mutex, and today one goroutine reads
// them. The lock is what makes a second one safe, and without a test that has
// two, removing it costs nothing that anything here would report.
func TestThePathsSurviveConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	defer stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))()
	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "status"), []byte("closed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				pcmRunning()
				forgetPCMPaths()
			}
		}()
	}
	wg.Wait()
}
