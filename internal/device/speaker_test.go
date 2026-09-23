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

const flingerDump = `Output thread 0xf5b6e008:
            numTracks=1 writeErrors=0 underruns=4 overruns=8
  Fast tracks: kMaxFastTracks=8 activeMask=0x1
  1 Tracks of which 1 are active
Input thread 0xf5136008:
  1 Tracks of which 1 are active
Reroute submix audio module:
`

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
		{"an input track is not playback",
			"Output thread 0x1:\n  0 Tracks of which 0 are active\n" +
				"Input thread 0x2:\n  3 Tracks of which 3 are active\n", false, true},
		{"tracks present but none active",
			"Output thread 0x1:\n  2 Tracks of which 0 are active\n", false, true},
		{"the short form without an active count",
			"Output thread 0x1:\n  1 Tracks\n", true, true},
		{"the short form with no tracks",
			"Output thread 0x1:\n  0 Tracks\n", false, true},
		{"no output thread at all", "Input thread 0x2:\n  1 Tracks of which 1 are active\n", false, false},
		{"an output thread whose track lines do not parse",
			"Output thread 0x1:\n  Tracks: 1 (1 active)\n  Standby: no\n", false, false},
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
		{"an unparsed output thread before a later section",
			"Output thread 0x1:\n  Tracks: 1 (1 active)\n  Standby: no\n" +
				"Input thread 0x2:\n  1 Tracks of which 1 are active\n" +
				"Reroute submix audio module:\n", false, false},
		{"an active output thread beside an idle one",
			"Output thread 0x1:\n  1 Tracks of which 1 are active\n" +
				"Output thread 0x2:\n  0 Tracks of which 0 are active\n", true, true},
		{"an idle output thread beside an active one",
			"Output thread 0x1:\n  0 Tracks of which 0 are active\n" +
				"Output thread 0x2:\n  1 Tracks of which 1 are active\n", true, true},
		{"a blank line does not end the thread",
			"Output thread 0x1:\n\n  1 Tracks of which 1 are active\n", true, true},
		{"a tab-indented line does not end the thread",
			"Output thread 0x1:\n\tStandby: no\n  1 Tracks of which 1 are active\n", true, true},
		{"a track count that is not at the start of its line",
			"Output thread 0x1:\n  AudioStreamOut: 7 Tracks of which 7 are active\n" +
				"  0 Tracks of which 0 are active\n", false, true},
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

func TestAnEmptyGlobIsRetried(t *testing.T) {
	dir := t.TempDir()
	restore := stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))
	defer restore()

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
			defer func(was dumpsys) { *speakerDump = was }(*speakerDump)
			speakerDump.command = func(context.Context) ([]byte, error) { return tt.out, tt.err }

			playing, ok := SpeakerPlaying()
			if playing != tt.playing || ok != tt.ok {
				t.Errorf("SpeakerPlaying = (%v, %v), want (%v, %v)", playing, ok, tt.playing, tt.ok)
			}
		})
	}
}

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
	defer func(was dumpsys) { *speakerDump = was }(*speakerDump)
	speakerDump.command = func(context.Context) ([]byte, error) {
		forked = true
		return []byte(flingerDump), nil
	}

	playing, ok := SpeakerPlaying()
	if playing || !ok {
		t.Errorf("SpeakerPlaying = (%v, %v), want (false, true)", playing, ok)
	}
	if forked {
		t.Error("a closed substream still forked dumpsys, which is the whole cost of this reading")
	}
}

func TestThePCMRefreshIsTheOneThatShips(t *testing.T) {
	speakerMu.Lock()
	got := pcmRefresh
	speakerMu.Unlock()
	if got < 30*time.Second {
		t.Errorf("pcmRefresh is %v, which globs oftener than the reading it guards is worth", got)
	}
	if got > 5*time.Minute {
		t.Errorf("pcmRefresh is %v, so a partial set survives that long reporting silence", got)
	}
}

func TestTheSpeakerReadBudgetIsBounded(t *testing.T) {
	if got := SpeakerReadBudget(); got != speakerDump.timeout+speakerDump.wait {
		t.Errorf("SpeakerReadBudget is %v, want the deadline plus the wait delay (%v)",
			got, speakerDump.timeout+speakerDump.wait)
	}
	if SpeakerReadBudget() >= 500*time.Millisecond {
		t.Errorf("SpeakerReadBudget is %v, which does not fit inside the half second it is "+
			"read on", SpeakerReadBudget())
	}
	if SpeakerReadBudget() <= 100*time.Millisecond {
		t.Errorf("SpeakerReadBudget is %v against a read measured at 18.7ms, which is too little "+
			"headroom on a loaded device", SpeakerReadBudget())
	}
}

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

	write("pcm0p", "closed\n")
	if running, ok := pcmRunning(); running || !ok {
		t.Fatalf("the early set reads as (%v, %v), want (false, true)", running, ok)
	}

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
	if running, ok := pcmRunning(); running || !ok {
		t.Errorf("after the vanished path, pcmRunning is (%v, %v), want (false, true): the "+
			"stale set was never dropped", running, ok)
	}
}

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

func TestOneSpeakerReadCannotOutlastItsBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "card0", "pcm23p", "sub0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "status"), []byte("state: RUNNING\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer stubPCM(t, filepath.Join(dir, "card*", "pcm*p", "sub*", "status"))()
	defer func(was dumpsys) { *speakerDump = was }(*speakerDump)

	speakerDump.timeout, speakerDump.wait = 100*time.Millisecond, 400*time.Millisecond
	speakerDump.argv = []string{"sh", "-c", "sleep 30 & sleep 30"}

	start := time.Now()
	playing, ok := SpeakerPlaying()
	elapsed := time.Since(start)

	if ok {
		t.Errorf("a read that never answered reported playing=%v as a reading", playing)
	}
	if elapsed < speakerDump.timeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	if limit := SpeakerReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, SpeakerReadBudget())
	}
}

func TestThePCMGlobWatchesPlaybackSubstreams(t *testing.T) {
	const want = "/proc/asound/card*/pcm*p/sub*/status"
	speakerMu.Lock()
	got := speakerPCMGlob
	speakerMu.Unlock()
	if got != want {
		t.Errorf("speakerPCMGlob is %q, want %q", got, want)
	}
}

func TestARunningSubstreamOutranksAnUnreadableOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which reads a 0000 file anyway")
	}
	dir := t.TempDir()
	defer stubPCM(t, filepath.Join(dir, "card*/pcm*p/sub*/status"))()

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
