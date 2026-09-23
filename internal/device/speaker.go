package device

import (
	"bytes"
	"os"
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

var pcmRefresh = time.Minute

var speakerDump = newDumpsys("media.audio_flinger", 300*time.Millisecond, 100*time.Millisecond)

func SpeakerReadBudget() time.Duration { return speakerDump.budget() }

func SpeakerPlaying() (bool, bool) {
	running, ok := pcmRunning()
	if !ok {
		return false, false
	}
	if !running {
		return false, true
	}
	out, err := speakerDump.read()
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
		return false, false
	}
	return playing, true
}
