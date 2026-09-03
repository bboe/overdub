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

// Most of these tests are about which key reaches which end, and the chime the
// key-down callback stands for is neither.
func noDown(uint16, Mode) {}

// An Interceptor watching one key in one mode, which is what most of these
// tests need. Built by hand rather than through Open, which wants a real node.
func watching(code uint16, m Mode) *Interceptor {
	w := &watch{}
	w.mode.Store(int32(m))
	return &Interceptor{watched: map[uint16]*watch{code: w}}
}

// The mode a latch test flips to. Any other mode will do: what is under test is
// that the halves of one press do not take different routes, not which route.
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
	// The chime hangs off the first of these, and it is worth nothing if it
	// waits for the release: a hold would then be silent for as long as it was
	// held.
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
		evdev.Event{Type: evdev.EvKey, Code: testKey, Value: 0}, // a second release
	)), func(uint16, int32) error { return nil }, noDown,
		func(_ uint16, _ Mode, h time.Duration) { held = append(held, h) })

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
		if _, err := Open(p, "mtk-kpd", time.Second, map[uint16]Mode{138: ModeIntercept}); err == nil {
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
		_, err := Open(p, "mtk-kpd", 200*time.Millisecond, map[uint16]Mode{testKey: ModeIntercept})
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
	// The chime is what says the daemon has the button, so a press Alexa is
	// answering has to be silent as well as unreported.
	if downs != 0 {
		t.Errorf("a released button sounded %d times, want 0", downs)
	}
	if want := []emitted{{testKey, 1}, {testKey, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %v, want %v: a key nobody is holding has to reach Android", got, want)
	}
}

// The zero value is a held key, so a watch built without a word about its mode
// keeps it. The alternative fails the wrong way round: a daemon that gave a
// button back because a field was never set.
func TestTheZeroValueHoldsTheKey(t *testing.T) {
	if Mode((&watch{}).mode.Load()) != ModeIntercept {
		t.Error("a fresh watch is not intercepting, so it hands its key to Alexa")
	}
}

// A key nobody asked to watch is one the clone re-emits, which is pass through
// by another name. Reporting it as intercept would say the daemon holds a key
// it has never looked at.
func TestAnUnwatchedKeyPassesThrough(t *testing.T) {
	i := watching(testKey, ModeIntercept)
	if got := i.Mode(testKey + 1); got != ModePassThrough {
		t.Errorf("an unwatched key is in %v, want %v", got, ModePassThrough)
	}
	// And setting it is a no-op rather than a panic on a nil watch.
	i.SetMode(testKey+1, ModeIntercept)
	if got := i.Mode(testKey + 1); got != ModePassThrough {
		t.Errorf("an unwatched key was set to %v", got)
	}
}

// Two keys on one node, each with its own mode: the whole reason a watch holds
// the mode rather than the Interceptor. Mute ships in monitor while the action
// button is intercepted, so this is the shipped arrangement rather than a
// contrived one.
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
	// The intercepted key reaches Android not at all; the monitored one does,
	// interleaved with it, and both are reported.
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
		start   Mode
		want    []emitted
		presses int
	}{
		// Latched intercepting: the release still reports the press, and
		// nothing of it reaches Android, which was never told it began.
		{"let go while held", ModeIntercept, nil, 1},
		// Latched passing: Android already has the key-down, so it gets the
		// key-up too rather than being left holding BUTTON_MODE.
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

// Autorepeat, which nothing else in the tree emits. It belongs to the press in
// flight: dropped while that press is being consumed, re-emitted while it is
// passing through, and dropped in every mode when there is no press of ours.
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
		// The key was down before the grab took it, so this end has no press to
		// hang the repeat on. Passing it on would give the clone a key-down of
		// its own, which is what the release cannot be relied on to end, so it
		// is dropped whatever the mode says.
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

// The three modes differ in two independent things: whether Android sees the
// key, and whether the caller hears about it. Monitor is the new one and the
// only combination that does both, which is what makes it worth a table rather
// than a third test that looks like the other two.
func TestEachModeEmitsAndReportsOrDoesNot(t *testing.T) {
	for _, tt := range []struct {
		mode      Mode
		wantEmit  []emitted
		wantDowns int
	}{
		// The key is ours: Android is never told, and the caller is.
		{ModeIntercept, nil, 1},
		// Alexa answers it as she always did, and Home Assistant hears too.
		{ModeMonitor, []emitted{{testKey, 1}, {testKey, 0}}, 1},
		// The clone does what the real node would, and nothing is reported.
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
			// Both halves or neither: a mode that chimed without reporting, or
			// reported without chiming, is one of them arriving alone.
			if downs != tt.wantDowns || presses != tt.wantDowns {
				t.Errorf("%v reported %d downs and %d presses, want %d of each",
					tt.mode, downs, presses, tt.wantDowns)
			}
		})
	}
}

// The mode names are what Home Assistant is offered and what it sends back, so
// a rename here is an option the daemon lists and then refuses to act on.
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

// A mode with no name answers with none. Without this a mode added later
// stringifies as intercept, so the select reports a button being held while it
// is being passed through, and the daemon agrees with itself all the way down.
func TestAModeWithNoNameHasNoName(t *testing.T) {
	if got := Mode(99).String(); got != "" {
		t.Errorf("an unrecognised mode is named %q, want the empty string", got)
	}
	// And every real one still has one, or the entity lists a blank option.
	for _, m := range []Mode{ModeIntercept, ModeMonitor, ModePassThrough} {
		if m.String() == "" {
			t.Errorf("mode %d has no name", m)
		}
	}
}

// A key held across the daemon starting: Android took the key-down from the
// real node before the grab, and only the release reaches here. There is no
// press of ours to report, and no release of ours clears that key-down either,
// since KeyDowns are tracked per device -- but the app layer does not look at
// the device, so a key Android is allowed to see still gets its release.
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
			// Nothing is reported either way: there was no press of ours.
			if presses != 0 {
				t.Errorf("%v reported %d presses for a release with no press", tt.mode, presses)
			}
		})
	}
}

// The mode is read live for a stale event, since no press latched one, so two
// of them can read it differently. A repeat is a key-down as far as EventHub is
// concerned -- it reads any non-zero value as one -- so a repeat emitted and
// then a release swallowed leaves Android holding the key, which
// docs/architecture.md names as the bad direction: past six hundred
// milliseconds it is Alexa's setup mode. Dropping the stale repeat is what
// makes the pair impossible to split.
func TestAStaleRepeatCannotStrandAKeyDownOnTheClone(t *testing.T) {
	i := watching(testKey, ModeMonitor)
	// Home Assistant flips the key once the repeat has been routed and before
	// the release arrives, which is the window between the two halves of a
	// press this end never saw begin.
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

// The flip runs at the start of the second read rather than the end of the
// first: route reads an event and then acts on it, so flipping when the first
// read returns lands before the repeat has been routed at all, and both events
// then read the new mode. That is the version of this test that cannot fail.
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
