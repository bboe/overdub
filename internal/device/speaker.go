package device

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var speakerPCMGlob = "/proc/asound/card*/pcm*p/sub*/status"

var (
	speakerMu  sync.Mutex
	pcmPaths   []string
	pcmGlobbed time.Time
)

// How long a resolved set of substream paths is trusted. Keeping one forever is
// what the cost argues for, but a set resolved while ALSA is still registering
// is readable and non-empty and so would never be reconsidered, and one missing
// pcm23p reports confident silence for the rest of the boot. A glob a minute is
// 5.6ms against sampling that costs 2.0ms twice a second.
var pcmRefresh = time.Minute

var (
	speakerReadTimeout = 300 * time.Millisecond
	speakerWaitDelay   = 100 * time.Millisecond

	speakerArgv = []string{"/system/bin/dumpsys", "media.audio_flinger"}

	speakerCommand = func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, speakerArgv[0], speakerArgv[1:]...)
		cmd.WaitDelay = speakerWaitDelay
		return cmd.Output()
	}
)

func SpeakerReadBudget() time.Duration { return speakerReadTimeout + speakerWaitDelay }

func SpeakerPlaying() (bool, bool) {
	running, ok := pcmRunning()
	if !ok {
		return false, false
	}
	if !running {
		return false, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), speakerReadTimeout)
	defer cancel()
	out, err := speakerCommand(ctx)
	if err != nil {
		return false, false
	}
	playing, recognised := hasOutputTrack(out)
	if !recognised {
		return false, false
	}
	return playing, true
}

func pcmStatusPaths() []string {
	speakerMu.Lock()
	defer speakerMu.Unlock()
	if len(pcmPaths) == 0 || time.Since(pcmGlobbed) >= pcmRefresh {
		pcmPaths, _ = filepath.Glob(speakerPCMGlob)
		pcmGlobbed = time.Now()
	}
	return pcmPaths
}

func pcmRunning() (bool, bool) {
	paths := pcmStatusPaths()
	if len(paths) == 0 {
		return false, false
	}
	unread := false
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			unread = true
			continue
		}
		if bytes.Contains(b, []byte("state: RUNNING")) {
			return true, true
		}
	}
	if unread {
		// A path that has gone is a stale set, and the cache only re-globs an
		// empty one. Without this a substream disappearing wedges the reading
		// at no-reading for the rest of the boot.
		forgetPCMPaths()
		return false, false
	}
	return false, true
}

func forgetPCMPaths() {
	speakerMu.Lock()
	defer speakerMu.Unlock()
	pcmPaths = nil
}

var trackLine = regexp.MustCompile(`^\s*(\d+) Tracks(?: of which (\d+) are active)?`)

func hasOutputTrack(dump []byte) (playing, recognised bool) {
	var output, counted, sawOutput bool
	understood := true
	// Every output thread has to be accounted for, not just one of them: a
	// dump half of which did not parse is one this no longer understands.
	endThread := func() {
		if output && !counted {
			understood = false
		}
	}
	for _, line := range strings.Split(string(dump), "\n") {
		switch {
		case strings.HasPrefix(line, "Output thread "):
			endThread()
			output, counted, sawOutput = true, false, true
		case line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t"):
			// Any other section at the left margin ends the thread's own
			// lines, so a track count below it is not the thread's.
			endThread()
			output = false
		case output:
			if m := trackLine.FindStringSubmatch(line); m != nil {
				counted = true
				n := m[1]
				if m[2] != "" {
					n = m[2]
				}
				if n != "0" {
					playing = true
				}
			}
		}
	}
	endThread()
	if !sawOutput || !understood {
		// Half a dump understood is not half an answer: a thread that says
		// something is playing does not excuse one whose lines did not parse.
		return false, false
	}
	return playing, true
}
