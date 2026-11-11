package sendspin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const lastEvery = time.Minute

type LastPlayback struct {
	path  string
	every time.Duration

	mu      sync.Mutex
	server  []byte
	wrote   time.Time
	pending *time.Timer
	failed  error

	writing sync.Mutex
}

func LoadLastPlayback(path string) (*LastPlayback, error) {
	l := &LastPlayback{path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if !info.Mode().IsRegular() {
		return l, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	if len(raw) != keyLen || allZero(raw) {
		return l, fmt.Errorf("%s is not one %d-byte server key", path, keyLen)
	}
	l.server = raw
	return l, nil
}

func (l *LastPlayback) id() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.server == nil {
		return ""
	}
	return EncodeID(l.server)
}

func (l *LastPlayback) set(server []byte) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	failed := l.failed
	l.failed = nil
	if bytes.Equal(l.server, server) {
		l.mu.Unlock()
		return failed
	}
	l.server = bytes.Clone(server)
	if l.path == "" || l.pending != nil {
		l.mu.Unlock()
		return failed
	}
	if wait := waitOr(l.every, lastEvery) - time.Since(l.wrote); wait > 0 {
		l.pending = time.AfterFunc(wait, func() {
			if err := l.write(); err != nil {
				l.mu.Lock()
				l.failed = err
				l.mu.Unlock()
			}
		})
		l.mu.Unlock()
		return failed
	}
	l.wrote = time.Now()
	l.mu.Unlock()
	return errors.Join(failed, l.write())
}

func (l *LastPlayback) write() error {
	l.writing.Lock()
	defer l.writing.Unlock()
	l.mu.Lock()
	l.pending = nil
	l.wrote = time.Now()
	server := bytes.Clone(l.server)
	l.mu.Unlock()
	return replaceFile(l.path, server)
}
