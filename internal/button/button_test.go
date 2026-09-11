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

func noDown(uint16, Mode) {}

func watching(code uint16, m Mode) *Interceptor {
	w := &watch{}
	w.mode.Store(int32(m))
	return &Interceptor{watched: map[uint16]*watch{code: w}}
}

func other(m Mode) Mode {
	if m == ModeIntercept {
		return ModePassThrough
	}
	return ModeIntercept
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
	var order []string
	i := watching(testKey, ModeIntercept)
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: 113, Value: 1}, // mute
		evdev.Event{Type: evdev.EvKey, Code: 113, Value: 0},
		evdev.Event{}, // EV_SYN, which the clone does not advertise
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, func(uint16, Mode) { order = append(order, "down") },
		func(_ uint16, _ Mode, h time.Duration) {
			order = append(order, "press")
			held = append(held, h)
		})

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
	if want := []string{"down", "press"}; !reflect.DeepEqual(order, want) {
		t.Errorf("the press reported %v, want %v", order, want)
	}
}

func TestRouteIgnoresAReleaseWithNoPress(t *testing.T) {
	var got []emitted
	presses := 0
	i := watching(testKey, ModeIntercept)
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, noDown, func(uint16, Mode, time.Duration) { presses++ })

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
	i := watching(testKey, ModeIntercept)
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: 114, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: 115, Value: 1},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return boom
	}, noDown, func(uint16, Mode, time.Duration) {})

	if !errors.Is(err, boom) {
		t.Fatalf("route returned %v, want %v: a dead clone holds the grab", err, boom)
	}
	if want := []emitted{{114, 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: the loop must stop at the first failure", got, want)
	}
}

func TestRouteIgnoresASecondReleaseAfterAPress(t *testing.T) {
	i := watching(testKey, ModeIntercept)
	var held []time.Duration
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
	)), func(uint16, int32) error { return nil }, noDown,
		func(_ uint16, _ Mode, h time.Duration) { held = append(held, h) })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if len(held) != 1 {
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
		if _, err := Open(p, "mtk-kpd", time.Second, map[uint16]Mode{138: ModeIntercept}); err == nil {
			t.Fatal("Open accepted a regular file as an input node")
		}
	}

	fails()
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
		_, err := Open(p, "mtk-kpd", 200*time.Millisecond, map[uint16]Mode{testKey: ModeIntercept})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open returned nil for a node that never appeared")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open was still waiting well past the limit it was given")
	}
}

func TestAReleasedButtonPassesThroughInsteadOfBeingActedOn(t *testing.T) {
	var got []emitted
	presses, downs := 0, 0
	i := watching(testKey, ModeIntercept)
	i.SetMode(testKey, ModePassThrough)
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, func(uint16, Mode) { downs++ }, func(uint16, Mode, time.Duration) { presses++ })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if presses != 0 {
		t.Errorf("a released button reported %d presses, want 0", presses)
	}
	if downs != 0 {
		t.Errorf("a released button sounded %d times, want 0", downs)
	}
	if want := []emitted{{testKey, 1}, {testKey, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: a key nobody is holding has to reach Android", got, want)
	}
}

func TestTheZeroValueHoldsTheKey(t *testing.T) {
	if Mode((&watch{}).mode.Load()) != ModeIntercept {
		t.Error("a fresh watch is not intercepting, so it hands its key to Alexa")
	}
}

func TestAnUnwatchedKeyPassesThrough(t *testing.T) {
	i := watching(testKey, ModeIntercept)
	if got := i.Mode(testKey + 1); got != ModePassThrough {
		t.Errorf("an unwatched key is in %v, want %v", got, ModePassThrough)
	}
	i.SetMode(testKey+1, ModeIntercept)
	if got := i.Mode(testKey + 1); got != ModePassThrough {
		t.Errorf("an unwatched key was set to %v", got)
	}
}

func TestTwoKeysHoldSeparateModes(t *testing.T) {
	const otherKey = testKey + 7
	aw, bw := &watch{}, &watch{}
	aw.mode.Store(int32(ModeIntercept))
	bw.mode.Store(int32(ModeMonitor))
	i := &Interceptor{watched: map[uint16]*watch{testKey: aw, otherKey: bw}}

	var got []emitted
	reported := map[uint16]int{}
	err := i.route(bytes.NewReader(events(
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: otherKey, Value: 1},
		evdev.Event{Type: evdev.EvKey, Code: otherKey, Value: 0},
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
	)), func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, func(uint16, Mode) {}, func(code uint16, _ Mode, _ time.Duration) { reported[code]++ })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if want := []emitted{{otherKey, 1}, {otherKey, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v", got, want)
	}
	if reported[testKey] != 1 || reported[otherKey] != 1 {
		t.Errorf("reported %v, want one press of each", reported)
	}
}

func TestSetModeIsReadBack(t *testing.T) {
	i := watching(testKey, ModeIntercept)
	for _, want := range []Mode{ModePassThrough, ModeMonitor, ModeIntercept, ModePassThrough} {
		i.SetMode(testKey, want)
		if got := i.Mode(testKey); got != want {
			t.Errorf("SetMode(%v) reads back as %v", want, got)
		}
	}
}

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

func TestATogglePartWayThroughAPressDoesNotSplitIt(t *testing.T) {
	for _, tt := range []struct {
		name    string
		start   Mode
		want    []emitted
		presses int
	}{
		{"let go while held", ModeIntercept, nil, 1},
		{"taken while held", ModePassThrough, []emitted{{testKey, 1}, {testKey, 0}}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []emitted
			presses := 0
			i := watching(testKey, ModeIntercept)
			i.SetMode(testKey, tt.start)
			r := &flipAfter{
				r: bytes.NewReader(events(
					evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
					evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
				)),
				at:   2,
				flip: func() { i.SetMode(testKey, other(tt.start)) },
			}
			err := i.route(r, func(code uint16, value int32) error {
				got = append(got, emitted{code, value})
				return nil
			}, noDown, func(uint16, Mode, time.Duration) { presses++ })

			if !errors.Is(err, io.EOF) {
				t.Fatalf("route returned %v, want io.EOF", err)
			}
			if i.Mode(testKey) == tt.start {
				t.Fatal("the mode never changed, so this test proves nothing")
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

func TestAutorepeatFollowsThePressItBelongsTo(t *testing.T) {
	const repeat = 2
	for _, tt := range []struct {
		name string
		mode Mode
		send []evdev.Event
		want []emitted
	}{
		{"consumed press swallows its repeats", ModeIntercept, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: 1},
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
			{Type: evdev.EvKey, Code: testKey, Value: 0},
		}, nil},
		{"passed-through press carries its repeats", ModePassThrough, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: 1},
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
			{Type: evdev.EvKey, Code: testKey, Value: 0},
		}, []emitted{{testKey, 1}, {testKey, repeat}, {testKey, 0}}},
		{"a repeat with no press of ours is dropped", ModePassThrough, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
		}, nil},
		{"an intercepted repeat with no press of ours is dropped", ModeIntercept, []evdev.Event{
			{Type: evdev.EvKey, Code: testKey, Value: repeat},
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []emitted
			i := watching(testKey, ModeIntercept)
			i.SetMode(testKey, tt.mode)
			err := i.route(bytes.NewReader(events(tt.send...)), func(code uint16, value int32) error {
				got = append(got, emitted{code, value})
				return nil
			}, noDown, func(uint16, Mode, time.Duration) {})
			if !errors.Is(err, io.EOF) {
				t.Fatalf("route returned %v, want io.EOF", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("emitted %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEachModeEmitsAndReportsOrDoesNot(t *testing.T) {
	for _, tt := range []struct {
		mode      Mode
		wantEmit  []emitted
		wantDowns int
	}{
		{ModeIntercept, nil, 1},
		{ModeMonitor, []emitted{{testKey, 1}, {testKey, 0}}, 1},
		{ModePassThrough, []emitted{{testKey, 1}, {testKey, 0}}, 0},
	} {
		t.Run(tt.mode.String(), func(t *testing.T) {
			var got []emitted
			downs, presses := 0, 0
			i := watching(testKey, ModeIntercept)
			i.SetMode(testKey, tt.mode)
			err := i.route(bytes.NewReader(events(
				evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 1},
				evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
			)), func(code uint16, value int32) error {
				got = append(got, emitted{code, value})
				return nil
			}, func(uint16, Mode) { downs++ }, func(uint16, Mode, time.Duration) { presses++ })

			if !errors.Is(err, io.EOF) {
				t.Fatalf("route returned %v, want io.EOF", err)
			}
			if !reflect.DeepEqual(got, tt.wantEmit) {
				t.Errorf("%v emitted %v, want %v", tt.mode, got, tt.wantEmit)
			}
			if downs != tt.wantDowns || presses != tt.wantDowns {
				t.Errorf("%v reported %d downs and %d presses, want %d of each",
					tt.mode, downs, presses, tt.wantDowns)
			}
		})
	}
}

func TestTheModeNamesAreTheOnesOffered(t *testing.T) {
	for _, tt := range []struct {
		mode Mode
		want string
	}{
		{ModeIntercept, "intercept"},
		{ModeMonitor, "monitor"},
		{ModePassThrough, "pass through"},
	} {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("mode %d is named %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestAModeWithNoNameHasNoName(t *testing.T) {
	if got := Mode(99).String(); got != "" {
		t.Errorf("an unrecognised mode is named %q, want the empty string", got)
	}
	for _, m := range []Mode{ModeIntercept, ModeMonitor, ModePassThrough} {
		if m.String() == "" {
			t.Errorf("mode %d has no name", m)
		}
	}
}

func TestAReleaseWithNoPressStillReachesAndroidUnlessIntercepted(t *testing.T) {
	for _, tt := range []struct {
		mode Mode
		want []emitted
	}{
		{ModeIntercept, nil},
		{ModeMonitor, []emitted{{testKey, 0}}},
		{ModePassThrough, []emitted{{testKey, 0}}},
	} {
		t.Run(tt.mode.String(), func(t *testing.T) {
			var got []emitted
			presses := 0
			i := watching(testKey, tt.mode)
			err := i.route(bytes.NewReader(events(
				evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
			)), func(code uint16, value int32) error {
				got = append(got, emitted{code, value})
				return nil
			}, noDown, func(uint16, Mode, time.Duration) { presses++ })

			if !errors.Is(err, io.EOF) {
				t.Fatalf("route returned %v, want io.EOF", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("%v emitted %v, want %v", tt.mode, got, tt.want)
			}
			if presses != 0 {
				t.Errorf("%v reported %d presses for a release with no press", tt.mode, presses)
			}
		})
	}
}

func TestAStaleRepeatCannotStrandAKeyDownOnTheClone(t *testing.T) {
	i := watching(testKey, ModeMonitor)
	r := &flipsBeforeTheRelease{
		r: bytes.NewReader(events(
			evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 2},
			evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0},
		)),
		flip: func() { i.SetMode(testKey, ModeIntercept) },
	}

	var got []emitted
	err := i.route(r, func(code uint16, value int32) error {
		got = append(got, emitted{code, value})
		return nil
	}, noDown, func(uint16, Mode, time.Duration) {})

	if !errors.Is(err, io.EOF) {
		t.Fatalf("route returned %v, want io.EOF", err)
	}
	if got != nil {
		t.Errorf("emitted %v, want nothing: a key-down the release cannot end", got)
	}
}

type flipsBeforeTheRelease struct {
	r     io.Reader
	flip  func()
	reads int
}

func (f *flipsBeforeTheRelease) Read(p []byte) (int, error) {
	f.reads++
	if f.reads == 2 {
		f.flip()
	}
	return f.r.Read(p)
}

func TestAPressIsBothHalvesOfAKey(t *testing.T) {
	var got []evdev.Event
	err := press(func(code uint16, value int32) error {
		got = append(got, evdev.Event{Code: code, Value: value})
		return nil
	}, 113)
	if err != nil {
		t.Fatalf("press: %v", err)
	}
	want := []evdev.Event{{Code: 113, Value: 1}, {Code: 113, Value: 0}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("press emitted %v, want a down then an up: %v", got, want)
	}
}

func TestAPressThatCannotStartDoesNotEmitTheRelease(t *testing.T) {
	calls := 0
	err := press(func(uint16, int32) error {
		calls++
		return errors.New("uinput: write: bad file descriptor")
	}, 113)
	if err == nil {
		t.Fatal("a press whose key-down failed reported success")
	}
	if calls != 1 {
		t.Errorf("the press made %d calls, want to stop after the down that failed", calls)
	}
}

func TestAReleaseThatFailedIsAStuckKey(t *testing.T) {
	calls := 0
	err := press(func(uint16, int32) error {
		calls++
		if calls == 2 {
			return errors.New("uinput: write: bad file descriptor")
		}
		return nil
	}, 113)
	if !errors.Is(err, ErrKeyStuck) {
		t.Errorf("a press whose release failed gave %v, want an ErrKeyStuck the caller can act on", err)
	}

	err = press(func(uint16, int32) error {
		return errors.New("uinput: write: bad file descriptor")
	}, 113)
	if !errors.Is(err, ErrKeyStuck) {
		t.Errorf("a press whose key-down failed gave %v, want an ErrKeyStuck: the SYN may "+
			"have been the half that failed, and Android acts on the key before it", err)
	}
}
