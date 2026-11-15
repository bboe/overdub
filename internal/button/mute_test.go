package button

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
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

func carrying(keys ...uint16) *Interceptor {
	i := watching(testKey, ModeIntercept)
	i.clone, i.keys = &evdev.Uinput{}, append([]uint16{testKey}, keys...)
	return i
}

func TestVolumeKeysOnTheActionNodeAreHeldInsideItsGrab(t *testing.T) {
	i := carrying(KeyVolumeUp, KeyVolumeDown)
	h, err := i.HoldVolume()
	if err != nil {
		t.Fatal(err)
	}
	var got []emitted
	err = i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeUp, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeUp, Value: 2},
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeUp, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, noDown, func(uint16, Mode, time.Duration) {})
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("emitted %v while the keys were held: a press would move a level nobody"+
			" can hear", got)
	}
	if err := h.Wait(); err != nil {
		t.Errorf("the press was not reported to the guard: %v", err)
	}
}

func TestAPressHeldAcrossTheReleaseKeepsItsUp(t *testing.T) {
	i := carrying(KeyVolumeUp, KeyVolumeDown)
	h, err := i.HoldVolume()
	if err != nil {
		t.Fatal(err)
	}
	var got []emitted
	emit := func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}
	i.route(bytes.NewReader(events(evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 1})),
		emit, noDown, func(uint16, Mode, time.Duration) {})
	h.Close()
	i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 1},
	)), emit, noDown, func(uint16, Mode, time.Duration) {})
	if want := []emitted{{KeyVolumeDown, 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: the up of a swallowed down is swallowed too, and the"+
			" next press passes", got, want)
	}
	if err := h.Wait(); !errors.Is(err, ErrReleased) && err != nil {
		t.Errorf("a released guard waited with %v", err)
	}
}

func TestOnlyANodeWithBothVolumeKeysCanHoldThem(t *testing.T) {
	if _, err := carrying(KeyVolumeUp).HoldVolume(); err == nil {
		t.Error("a node without volume down held the volume keys")
	}
	w := watching(testKey, ModeMonitor)
	w.keys = []uint16{KeyVolumeUp, KeyVolumeDown}
	if _, err := w.HoldVolume(); err == nil {
		t.Error("a watched node held the volume keys; nothing grabs it, so they still reach Alexa")
	}
}

func TestAKeyHeldWhenTheMuteStartsRepeatsNowhere(t *testing.T) {
	i := carrying(KeyVolumeUp, KeyVolumeDown)
	var got []emitted
	emit := func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}
	i.route(bytes.NewReader(events(evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 1})),
		emit, noDown, func(uint16, Mode, time.Duration) {})
	if _, err := i.HoldVolume(); err != nil {
		t.Fatal(err)
	}
	i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 2},
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 2},
		evdev.Event{Type: evdev.EvKey, Code: KeyVolumeDown, Value: 0},
	)), emit, noDown, func(uint16, Mode, time.Duration) {})
	if want := []emitted{{KeyVolumeDown, 1}, {KeyVolumeDown, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: a repeat moves a level nobody can hear, and the"+
			" release of a down the clone saw must reach it", got, want)
	}
}
