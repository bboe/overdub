// Package untrustedlog bounds how much a remote party can make this daemon write to
// /data: how often, and how long each line. It does not escape what a peer
// supplied -- the call site still quotes it.
package untrustedlog

import (
	"log"
	"sync"
	"time"
)

const (
	Burst = 20

	total = 5000

	window    = time.Minute
	maxString = 64
)

func Cut(s string) string {
	if len(s) > maxString {
		return s[:maxString] + "..."
	}
	return s
}

type Log struct {
	Subject string

	mu        sync.Mutex
	windowEnd time.Time
	lines     int
	dropped   int
	written   int
}

func (l *Log) Printf(format string, args ...any) {
	dropped, allow, last := l.allow()
	if dropped > 0 {
		log.Printf("%s%d lines suppressed", l.prefix(), dropped)
	}
	if allow {
		log.Printf(format, args...)
	}
	if last {
		log.Printf("%s%d lines this run; nothing a peer does is logged again"+
			" until a restart", l.prefix(), total)
	}
}

func (l *Log) Written() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written
}

func (l *Log) prefix() string {
	if l.Subject == "" {
		return ""
	}
	return l.Subject + ": "
}

func (l *Log) allow() (dropped int, allow, last bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.written >= total {
		return 0, false, false
	}
	if now := time.Now(); now.After(l.windowEnd) {
		l.windowEnd = now.Add(window)
		l.lines = 0
		dropped, l.dropped = l.dropped, 0
	}
	if l.lines >= Burst {
		l.dropped++
		return dropped, false, false
	}
	l.lines++
	l.written++
	return dropped, true, l.written == total
}
