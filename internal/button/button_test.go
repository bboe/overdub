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

// The whole of what "not captured" means: the key reaches the clone, named
// mtk-kpd and carrying the same keylayout, so Android sees the press it would
// have seen with no daemon running. The grab is untouched -- releasing it would
// put a live original beside a live clone -- so this is the only route back.
func TestAReleasedButtonPassesThroughInsteadOfBeingActedOn(t *testing.T) {
	var got []emitted
	presses := 0
	i := &Interceptor{consume: testKey}
	i.SetCaptured(false)
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, func(time.Duration) { presses++ })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if presses != 0 {
		t.Errorf("a released button reported %d presses, want 0", presses)
	}
	if want := []emitted{{testKey, 1}, {testKey, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: a key nobody is holding has to reach Android", got, want)
	}
}

// The zero value is a captured button, so an Interceptor built without a word
// about capture keeps the key. The alternative fails the wrong way round: a
// daemon that gave the button back because a field was never set.
func TestTheZeroValueHoldsTheButton(t *testing.T) {
	if !(&Interceptor{}).Captured() {
		t.Error("a fresh Interceptor is not capturing, so it hands the action button to Alexa")
	}
}

func TestSetCapturedIsReadBack(t *testing.T) {
	i := &Interceptor{consume: testKey}
	for _, want := range []bool{false, true, false} {
		i.SetCaptured(want)
		if got := i.Captured(); got != want {
			t.Errorf("SetCaptured(%v) reads back as %v", want, got)
		}
	}
}

// A reader that flips the capture flag as it hands over the nth event, so the
// toggle lands after the event before it has been acted on and before this one
// is: route reads exactly one event and then processes it. Flipping on the
// first read would land before the press was latched at all, which is the
// unlatched behaviour rather than the latched one.
type flipAfter struct {
	r    io.Reader
	n    int
	at   int
	flip func()
}

func (f *flipAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if f.n++; f.n == f.at {
		f.flip()
	}
	return n, err
}

// A press is decided at its key-down and stays decided until the key-up. Both
// halves of one press have to take the same route, and the two ways of getting
// that wrong are not symmetric: a release with no press is a key Android shrugs
// at, while a press with no release is BUTTON_MODE held down for ever.
func TestATogglePartWayThroughAPressDoesNotSplitIt(t *testing.T) {
	for _, tt := range []struct {
		name    string
		start   bool
		want    []emitted
		presses int
	}{
		// Latched captured: the release still reports the press, and nothing
		// of it reaches Android, which was never told it began.
		{"let go while held", true, nil, 1},
		// Latched passing: Android already has the key-down, so it gets the
		// key-up too rather than being left holding BUTTON_MODE.
		{"taken while held", false, []emitted{{testKey, 1}, {testKey, 0}}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []emitted
			presses := 0
			i := &Interceptor{consume: testKey}
			i.SetCaptured(tt.start)
			r := &flipAfter{
				r: bytes.NewReader(events(
					evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
					evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
				)),
				at:   2,
				flip: func() { i.SetCaptured(!tt.start) },
			}
			err := i.route(r, func(code uint16, value int32) error {
				got = append(got, emitted{code, value})
				return nil
			}, func(time.Duration) { presses++ })

			if !errors.Is(err, io.EOF) {
				t.Fatalf("route returned %v, want io.EOF", err)
			}
			if i.Captured() == tt.start {
				t.Fatal("the flag never flipped, so this test proves nothing")
			}
			if presses != tt.presses {
				t.Errorf("reported %d presses, want %d", presses, tt.presses)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("emitted %v, want %v", got, tt.want)
			}
		})
	}
}

// Autorepeat, which nothing else in the tree emits. It belongs to the press in
// flight: dropped while that press is being consumed, re-emitted while it is
// passing through, and dropped entirely when there is no press of ours, since
// then Android was never told the key went down.
func TestAutorepeatFollowsThePressItBelongsTo(t *testing.T) {
	const repeat = 2
	for _, tt := range []struct {
		name     string
		captured bool
		send     []evdev.Event
		want     []emitted
	}{
		{"consumed press swallows its repeats", true, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: 1},
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
			{Type: evdev.EvKey, Code: testKey, Value: 0},
		}, nil},
		{"passed-through press carries its repeats", false, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: 1},
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
			{Type: evdev.EvKey, Code: testKey, Value: 0},
		}, []emitted{{testKey, 1}, {testKey, repeat}, {testKey, 0}}},
		// The key was down before the grab took it, so Android has no press of
		// ours to hang a repeat on.
		{"a repeat with no press of ours is dropped", false, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []emitted
			i := &Interceptor{consume: testKey}
			i.SetCaptured(tt.captured)
			err := i.route(bytes.NewReader(events(tt.send...)), func(code uint16, value int32) error {
				got = append(got, emitted{code, value})
				return nil
			}, func(time.Duration) {})
			if !errors.Is(err, io.EOF) {
				t.Fatalf("route returned %v, want io.EOF", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("emitted %v, want %v", got, tt.want)
			}
		})
	}
}
