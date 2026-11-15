package alexa

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	duckPercent     = 20
	deepDuckPercent = 10
	fullPercent     = 100
	holdFor         = 15 * time.Minute

	acquireMark = "FocusManager:acquireChannel:channelName="
	releaseMark = "FocusManager:releaseChannel:channelName="
	tagMark     = " PuffinApp: "

	musicChannel = "Content"
	speech       = "SpeechSynthesizer"
	interfaceKey = "interface="
)

func focusArgv(from time.Time) []string {
	since := fmt.Sprintf("%d.%03d", from.Unix(), from.Nanosecond()/int(time.Millisecond))
	return []string{"/system/bin/logcat", "-T", since, "-v", "threadtime", "-s", "PuffinApp"}
}

type focusHolder struct {
	executor string
	iface    string
	until    time.Time
}

type FocusWatcher struct {
	OnLevel func(percent int)
	OnMusic func(playing bool)
	OnSound func(channel string)

	notify  sync.Mutex
	mu      sync.Mutex
	holders map[string]focusHolder
	level   int
	music   bool
	sound   string
	pid     string
	now     func() time.Time
	alive   func(pid string) bool
}

func (w *FocusWatcher) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *FocusWatcher) Run() {
	go w.sweep()
	keepFollowing("focus watcher", focusArgv, w.line, w.reset)
}

func (w *FocusWatcher) sweep() {
	for range time.Tick(sweepEvery) {
		w.expire()
	}
}

func (w *FocusWatcher) read(r io.Reader) error {
	return readLines(r, w.line)
}

func (w *FocusWatcher) line(line string) {
	if pid := pidOf(line); pid != "" {
		w.mu.Lock()
		was := w.pid
		w.mu.Unlock()
		switch {
		case pid == was:
		case was == "":
			w.change(func() { w.pid = pid })
		case !w.running(was):
			log.Printf("focus watcher: PuffinApp is pid %s now, and %s is gone, so every"+
				" channel the old one held is released", pid, was)
			w.change(func() {
				clear(w.holders)
				w.pid = pid
			})
		}
	}
	if i := strings.Index(line, acquireMark); i >= 0 {
		channel, rest, _ := strings.Cut(line[i+len(acquireMark):], ",")
		iface, _ := strings.CutPrefix(strings.TrimSpace(rest), interfaceKey)
		iface, _, _ = strings.Cut(iface, ",")
		if executor, ok := executorOf(line[:i]); ok {
			w.acquire(strings.TrimSpace(channel), executor, strings.TrimSpace(iface))
		}
		return
	}
	if i := strings.Index(line, releaseMark); i >= 0 {
		channel, _, _ := strings.Cut(line[i+len(releaseMark):], ",")
		if executor, ok := executorOf(line[:i]); ok {
			w.release(strings.TrimSpace(channel), executor)
		}
	}
}

func (w *FocusWatcher) running(pid string) bool {
	if w.alive != nil {
		return w.alive(pid)
	}
	_, err := os.Stat("/proc/" + pid)
	return err == nil
}

func pidOf(line string) string {
	i := strings.Index(line, tagMark)
	if i < 0 {
		return ""
	}
	fields := strings.Fields(line[:i])
	if len(fields) != 5 {
		return ""
	}
	return fields[2]
}

func executorOf(prefix string) (string, bool) {
	prefix = strings.TrimRight(prefix, " ")
	if !strings.HasSuffix(prefix, "]") {
		return "", false
	}
	open := strings.LastIndex(prefix, "[")
	if open < 0 {
		return "", false
	}
	executor := prefix[open+1 : len(prefix)-1]
	return executor, executor != ""
}

func (w *FocusWatcher) acquire(channel, executor, iface string) {
	var until time.Time
	switch {
	case percentFor(channel) < fullPercent:
		until = w.clock().Add(holdFor)
	case channel != musicChannel:
		return
	}
	w.change(func() {
		if w.holders == nil {
			w.holders = map[string]focusHolder{}
		}
		w.holders[channel] = focusHolder{executor: executor, iface: iface, until: until}
	})
}

func (w *FocusWatcher) release(channel, executor string) {
	w.change(func() {
		if h, ok := w.holders[channel]; ok && h.executor == executor {
			delete(w.holders, channel)
		}
	})
}

func (w *FocusWatcher) expire() {
	w.mu.Lock()
	pid := w.pid
	w.mu.Unlock()
	if pid != "" && !w.running(pid) {
		held := 0
		w.change(func() {
			held = len(w.holders)
			clear(w.holders)
			w.pid = ""
		})
		log.Printf("focus watcher: PuffinApp %s is gone, so the %d channels it held are"+
			" released", pid, held)
		return
	}
	dropped := 0
	w.change(func() {
		now := w.clock()
		for channel, h := range w.holders {
			if !h.until.IsZero() && now.After(h.until) {
				delete(w.holders, channel)
				dropped++
			}
		}
	})
	if dropped > 0 {
		log.Printf("focus watcher: %d channels were never released, so their hold"+
			" was dropped after %v", dropped, holdFor)
	}
}

func (w *FocusWatcher) reset() {
	w.change(func() {
		clear(w.holders)
	})
}

func (w *FocusWatcher) change(mutate func()) {
	w.notify.Lock()
	defer w.notify.Unlock()

	w.mu.Lock()
	mutate()
	level := fullPercent
	for channel := range w.holders {
		level = min(level, percentFor(channel))
	}
	_, music := w.holders[musicChannel]
	sound := soundOf(w.holders)
	was, wasMusic, wasSound := w.level, w.music, w.sound
	if was == 0 {
		was = fullPercent
	}
	w.level, w.music, w.sound = level, music, sound
	w.mu.Unlock()

	if level != was && w.OnLevel != nil {
		w.OnLevel(level)
	}
	if music != wasMusic && w.OnMusic != nil {
		w.OnMusic(music)
	}
	if sound != wasSound && w.OnSound != nil {
		w.OnSound(sound)
	}
}

func soundOf(holders map[string]focusHolder) string {
	for _, channel := range []string{"Alert", "Communications", "Dialog", musicChannel} {
		h, ok := holders[channel]
		if !ok || channel == "Dialog" && h.iface != speech {
			continue
		}
		return strings.ToLower(channel)
	}
	return ""
}

func percentFor(channel string) int {
	switch channel {
	case "Dialog":
		return duckPercent
	case "Alert", "Communications":
		return deepDuckPercent
	}
	return fullPercent
}
