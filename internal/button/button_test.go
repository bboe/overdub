package button

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/evdev"
)

// Deliberately not main's 138: a route that compares against a hardcoded
// keycode instead of the one it was given has to fail here.
const testKey = 211

func TestWaitForNodeReturnsWhenPresent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "event1")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForNode(p, 2*time.Second); err != nil {
		t.Errorf("waitForNode on an existing path: %v", err)
	}
}

func TestWaitForNodeWaitsOutItsLimitAndNoLonger(t *testing.T) {
	p := filepath.Join(t.TempDir(), "never")
	const limit = 300 * time.Millisecond
	start := time.Now()
	if err := waitForNode(p, limit); err == nil {
		t.Fatal("waitForNode returned nil for a path that never appeared")
	}
	waited := time.Since(start)
	if waited < limit {
		t.Errorf("waitForNode gave up after %v, want at least %v", waited, limit)
	}
	if waited > limit+500*time.Millisecond {
		t.Errorf("waitForNode overran its limit by %v", waited-limit)
	}
}

func TestWaitForNodeReturnsOnceTheNodeAppears(t *testing.T) {
	p := filepath.Join(t.TempDir(), "late")
	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			panic(err)
		}
	}()
	start := time.Now()
	if err := waitForNode(p, 10*time.Second); err != nil {
		t.Fatalf("waitForNode on a node that appeared late: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("waitForNode took %v to notice a node that appeared after 100ms", took)
	}
}

type emitted struct {
	code  uint16
	value int32
}

func events(list ...evdev.Event) []byte {
	b := make([]byte, 0, len(list)*evdev.EventSize)
	for _, e := range list {
		one := make([]byte, evdev.EventSize)
		binary.LittleEndian.PutUint16(one[8:], e.Type)
		binary.LittleEndian.PutUint16(one[10:], e.Code)
		binary.LittleEndian.PutUint32(one[12:], uint32(e.Value))
		b = append(b, one...)
	}
	return b
}

func TestRouteConsumesTheActionButtonAndPassesTheRest(t *testing.T) {
	var got []emitted
	var held []time.Duration
	i := &Interceptor{consume: testKey}
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: 113, Value: 1}, // mute
		evdev.Event{Type: evdev.EvKey, Code: 113, Value: 0},
		evdev.Event{}, // EV_SYN, which the clone does not advertise
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, func(h time.Duration) { held = append(held, h) })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF once the events run out", err)
	}
	if want := []emitted{{113, 1}, {113, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: the consumed keycode must not reach the clone", got, want)
	}
	if len(held) != 1 {
		t.Fatalf("reported %d presses, want 1", len(held))
	}
	if held[0] <= 0 || held[0] > time.Second {
		t.Errorf("press held %v, want a short positive duration", held[0])
	}
}

func TestRouteIgnoresAReleaseWithNoPress(t *testing.T) {
	var got []emitted
	presses := 0
	i := &Interceptor{consume: testKey}
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, func(time.Duration) { presses++ })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if presses != 0 {
		t.Errorf("a release with no press reported %d presses, want 0", presses)
	}
	if got != nil {
		t.Errorf("a release of the consumed key emitted %v, want nothing", got)
	}
}

func TestRouteStopsOnAFailedEmit(t *testing.T) {
	var got []emitted
	boom := errors.New("no such device")
	i := &Interceptor{consume: testKey}
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: 114, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: 115, Value: 1},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return boom
	}, func(time.Duration) {})

	if !errors.Is(err, boom) {
		t.Fatalf("route returned %v, want %v: a dead clone holds the grab", err, boom)
	}
	if want := []emitted{{114, 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: the loop must stop at the first failure", got, want)
	}
}

func TestRouteIgnoresASecondReleaseAfterAPress(t *testing.T) {
	i := &Interceptor{consume: testKey}
	var held []time.Duration
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0}, // a second release
	)), func(uint16, int32) error { return nil },
		func(h time.Duration) { held = append(held, h) })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if len(held) != 1 {
		// Without clearing pressedAt, the second release reports a press that
		// was held since the first one.
		t.Errorf("reported %d presses for one press and two releases, want 1", len(held))
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	names, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd here: %v", err)
	}
	return len(names)
}

func TestOpenClosesTheNodeItCannotUse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "event1")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fails := func() {
		t.Helper()
		if _, err := Open(p, 138, "mtk-kpd", time.Second); err == nil {
			t.Fatal("Open accepted a regular file as an input node")
		}
	}

	fails() // once to settle any lazily opened descriptor
	before := openFDs(t)
	for i := 0; i < 20; i++ {
		fails()
	}
	if after := openFDs(t); after > before {
		t.Errorf("20 failed Opens leaked %d descriptors; the node is not closed "+
			"when it cannot be used", after-before)
	}
}

func TestOpenGivesUpAfterTheWaitItIsGiven(t *testing.T) {
	p := filepath.Join(t.TempDir(), "never")
	done := make(chan error, 1)
	go func() {
		_, err := Open(p, testKey, "mtk-kpd", 200*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open returned nil for a node that never appeared")
		}
	case <-time.After(10 * time.Second):
		// The caller owns this decision; a wait of its own would ignore it.
		t.Fatal("Open was still waiting well past the limit it was given")
	}
}
