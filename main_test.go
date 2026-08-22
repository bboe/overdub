package main

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
	if waited > limit+2*time.Second {
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
	err := route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: actionKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: actionKey, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: 113, Value: 1}, // mute
		evdev.Event{Type: evdev.EvKey, Code: 113, Value: 0},
		evdev.Event{}, // EV_SYN, which the clone does not advertise
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, actionKey, func(h time.Duration) { held = append(held, h) })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF once the events run out", err)
	}
	if want := []emitted{{113, 1}, {113, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: keycode %d must not reach the clone", got, want, actionKey)
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
	err := route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: actionKey, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, actionKey, func(time.Duration) { presses++ })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if presses != 0 {
		t.Errorf("a release with no press reported %d presses, want 0", presses)
	}
	if got != nil {
		t.Errorf("a release of %d emitted %v, want nothing", actionKey, got)
	}
}

func TestRouteStopsOnAFailedEmit(t *testing.T) {
	var got []emitted
	boom := errors.New("no such device")
	err := route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: 114, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: 115, Value: 1},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return boom
	}, actionKey, func(time.Duration) {})

	if !errors.Is(err, boom) {
		t.Fatalf("route returned %v, want %v: a dead clone holds the grab", err, boom)
	}
	if want := []emitted{{114, 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: the loop must stop at the first failure", got, want)
	}
}
