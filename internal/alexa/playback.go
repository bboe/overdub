package alexa

import (
	"bufio"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	watchRetry = 10 * time.Second
	sweepEvery = 2 * time.Second

	// Longer than any clip anybody hands a Dot, and short enough that a playback
	// she starts and never reports the end of does not last the run.
	playingFor = 15 * time.Minute
)

var watchArgv = []string{"/system/bin/logcat", "-T", "1", "-v", "brief", "-s", "tts-Server", "tts-Playback"}

type PlaybackWatcher struct {
	OnStart func()
	OnEnd   func(ok bool, detail string)

	mu        sync.Mutex
	expecting bool
	lastError string
	deadline  time.Time
	now       func() time.Time
}

func (w *PlaybackWatcher) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *PlaybackWatcher) Expect(timeout time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.expecting = true
	w.lastError = ""
	w.deadline = w.clock().Add(timeout)
}

func (w *PlaybackWatcher) Extend(timeout time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.expecting {
		return
	}
	w.deadline = w.clock().Add(timeout)
}

func (w *PlaybackWatcher) Cancel() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.expecting = false
}

func (w *PlaybackWatcher) Run() {
	go w.sweep()
	said := false
	for {
		start := time.Now()
		err := w.tail()
		var say bool
		say, said = shouldSay(said, time.Since(start), err)
		if say {
			log.Printf("playback watcher: %v; retrying every %v, and saying so once",
				err, watchRetry)
		}
		time.Sleep(watchRetry)
	}
}

func shouldSay(said bool, ranFor time.Duration, err error) (say, nowSaid bool) {
	if ranFor >= watchRetry {
		said = false
	}
	if err == nil {
		return false, said
	}
	if said {
		return false, true
	}
	return true, true
}

func (w *PlaybackWatcher) sweep() {
	for range time.Tick(sweepEvery) {
		w.expire()
	}
}

func (w *PlaybackWatcher) expire() {
	w.mu.Lock()
	expired := w.expecting && w.clock().After(w.deadline)
	if expired {
		w.expecting = false
	}
	w.mu.Unlock()
	if expired && w.OnEnd != nil {
		w.OnEnd(false, "timed out waiting for Alexa to report playback")
	}
}

func (w *PlaybackWatcher) tail() error {
	cmd := exec.Command(watchArgv[0], watchArgv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	return w.read(stdout)
}

func (w *PlaybackWatcher) read(r io.Reader) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		w.line(sc.Text())
	}
	return sc.Err()
}

func (w *PlaybackWatcher) line(line string) {
	var (
		started, ended, ok bool
		detail             string
	)

	w.mu.Lock()
	if w.expecting {
		if strings.Contains(line, "cannot estimate length") || strings.Contains(line, "Exception") {
			w.lastError = strings.TrimSpace(line)
		}
		switch {
		case strings.Contains(line, "Playback started:") && strings.Contains(line, "SpeechSynthesizer"):
			started = true
			w.deadline = w.clock().Add(playingFor)
		case strings.Contains(line, "Playback ended:") && strings.Contains(line, "SpeechSynthesizer"):
			ended = true
			ok = strings.Contains(line, "SUCCESS")
			detail = w.lastError
			w.expecting = false
		}
	}
	w.mu.Unlock()

	if started && w.OnStart != nil {
		w.OnStart()
	}
	if ended && w.OnEnd != nil {
		w.OnEnd(ok, detail)
	}
}
