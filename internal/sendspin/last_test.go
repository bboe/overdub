package sendspin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheLastPlaybackFileIsReadOnlyWhenItIsOneKey(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, keyLen)
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := LoadLastPlayback(good); err != nil || l.id() != EncodeID(key) {
		t.Errorf("a good file loaded as %q, %v", l.id(), err)
	}
	if l, err := LoadLastPlayback(filepath.Join(dir, "missing")); err != nil || l.id() != "" {
		t.Errorf("a missing file loaded as %q, %v; want none and no error", l.id(), err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"short": key[:keyLen-1],
		"zeros": make([]byte, keyLen),
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if l, err := LoadLastPlayback(path); err == nil || l.id() != "" {
			t.Errorf("a %s file loaded as %q with no error", name, l.id())
		}
	}
	if l, err := LoadLastPlayback(link); err == nil || l.id() != "" {
		t.Errorf("a symlink was followed: %q, %v", l.id(), err)
	}
}

func TestTheLastPlaybackFileIsWrittenAtMostOnceAWhile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last")
	l := &LastPlayback{path: path, every: 200 * time.Millisecond}
	a, b, c := bytes.Repeat([]byte{1}, keyLen), bytes.Repeat([]byte{2}, keyLen),
		bytes.Repeat([]byte{3}, keyLen)
	for _, key := range [][]byte{a, b, c} {
		if err := l.set(key); err != nil {
			t.Fatal(err)
		}
	}
	if l.id() != EncodeID(c) {
		t.Errorf("held %q, want the newest server", l.id())
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, a) {
		t.Errorf("the file holds %x, %v; want the first write only: a peer that changes"+
			" the last server every activation would otherwise rewrite flash without bound",
			got, err)
	}
	waitFor(t, "the newest server to be written once the minute was up", func() bool {
		got, _ := os.ReadFile(path)
		return bytes.Equal(got, c)
	})
}
