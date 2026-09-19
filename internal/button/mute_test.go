package button

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/evdev"
)

func TestAVolumeKeyGoingDownEndsTheGuard(t *testing.T) {
	for _, code := range []uint16{KeyVolumeUp, KeyVolumeDown} {
		var buf bytes.Buffer
		buf.Write(events(evdev.Event{Type: evdev.EvKey, Code: code, Value: evdev.KeyPress}))
		if err := waitForVolumeKey(&buf); err != nil {
			t.Errorf("a %d going down did not end the guard: %v", code, err)
		}
	}
}

func TestAKeyGoingUpDoesNotEndTheGuard(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(events(evdev.Event{Type: evdev.EvKey, Code: KeyVolumeUp, Value: evdev.KeyRelease}))
	buf.Write(events(evdev.Event{Type: evdev.EvKey, Code: KeyVolumeUp, Value: evdev.KeyPress}))
	if err := waitForVolumeKey(&buf); err != nil {
		t.Fatalf("waitForVolumeKey: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("%d bytes were left unread, so the release ended the guard rather than"+
			" the press that followed it; the first press would then be spent on"+
			" letting go of a key nobody pressed while muted", buf.Len())
	}
}

func TestAGuardThatLosesItsNodeSaysSo(t *testing.T) {
	if err := waitForVolumeKey(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("a closed node ended the guard with %v, want EOF: the caller unmutes on"+
			" a nil error, so a lost grab would unmute with nobody pressing anything", err)
	}
}

func TestClosingAGuardEndsItsWait(t *testing.T) {
	node, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_ = volumeKeysOn(node)
	_ = evdev.Grab(node, true)
	g := &VolumeGuard{node: node}

	done := make(chan error, 1)
	go func() { done <- g.Wait() }()

	time.Sleep(50 * time.Millisecond)
	g.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a guard closed while waiting reported a press, so unmuting from" +
				" home assistant would unmute again on whatever it read next")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the wait: the goroutine, its thread and the open" +
			" node are leaked for the life of the daemon, and the reader wakes on a" +
			" later press to unmute a mute it does not own")
	}
	g.Close()
}

func TestANodeThatIsNotTheVolumeKeysIsRefused(t *testing.T) {
	node, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	defer peer.Close()

	if err := volumeKeysOn(node); err == nil {
		t.Error("a node that declares no volume keycodes was accepted; grabbing one" +
			" takes the wrong device exclusively and leaves a mute no press can lift")
	}
}
