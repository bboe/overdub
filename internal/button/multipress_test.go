package button

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

type fired struct {
	gesture Gesture
	count   int
	holdFor time.Duration
}

// Short enough to wait out, long enough that a loaded builder does not expire a
// run mid-test. The threshold is clear of the gap, as it is on the device.
const (
	testGap  = 20 * time.Millisecond
	testHold = 60 * time.Millisecond
)

func collector() (*MultiPress, func() []fired) { return collectorWith(testGap, testHold) }

func collectorWith(gap, hold time.Duration) (*MultiPress, func() []fired) {
	var mu sync.Mutex
	var got []fired
	m := NewMultiPress(gap, hold, func(g Gesture, count int, holdFor time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fired{g, count, holdFor})
	})
	return m, func() []fired {
		mu.Lock()
		defer mu.Unlock()
		return append([]fired(nil), got...)
	}
}

// A press too short to be a hold, which is what every press in a run is.
func (m *MultiPress) tap() {
	m.Down()
	m.Up(time.Millisecond)
}

func settle() { time.Sleep(4 * testGap) }

// One press is press_end, and only after the gap.
func TestOnePressIsPressEnd(t *testing.T) {
	m, got := collector()
	m.tap()
	if n := len(got()); n != 0 {
		t.Fatalf("a single press reported %d gestures before the gap passed, want 0", n)
	}
	settle()

	if want := []fired{{GesturePressEnd, 0, 0}}; !reflect.DeepEqual(got(), want) {
		t.Errorf("one press reported %v, want %v", got(), want)
	}
}

// A run is one gesture carrying its count, reported when the gap closes it.
func TestARunIsReportedOnceWhenItCloses(t *testing.T) {
	m, got := collector()
	for range 3 {
		m.tap()
	}
	if n := len(got()); n != 0 {
		t.Fatalf("three presses reported %v before the gap passed, want nothing", got())
	}
	settle()

	if want := []fired{{GestureMultiEnd, 3, 0}}; !reflect.DeepEqual(got(), want) {
		t.Errorf("three presses reported %v, want %v", got(), want)
	}
}

// The gap is what ends a run, so two runs separated by one are two runs.
func TestAGapEndsTheRun(t *testing.T) {
	m, got := collector()
	m.tap()
	settle()
	m.tap()
	settle()

	want := []fired{{GesturePressEnd, 0, 0}, {GesturePressEnd, 0, 0}}
	if !reflect.DeepEqual(got(), want) {
		t.Errorf("two presses either side of a gap reported %v, want %v", got(), want)
	}
}

// The point of driving this from the key: the hold reports while the key is
// still down, so an automation can act during it.
func TestAHoldIsReportedWhileTheKeyIsStillDown(t *testing.T) {
	m, got := collector()
	m.Down()
	time.Sleep(3 * testHold)

	if want := []fired{{GestureLongStart, 0, 0}}; !reflect.DeepEqual(got(), want) {
		t.Fatalf("a key held past the threshold reported %v before its release, want %v", got(), want)
	}
	m.Up(742 * time.Millisecond)

	want := []fired{{GestureLongStart, 0, 0}, {GestureLongEnd, 0, 742 * time.Millisecond}}
	if !reflect.DeepEqual(got(), want) {
		t.Errorf("a hold reported %v, want %v", got(), want)
	}
}

// The boundary, decided on the duration the release carries rather than on
// which goroutine won a race. A threshold far beyond the test's patience keeps
// the hold timer out of it entirely, so what is measured is the backstop.
func TestTheReleaseDecidesTheBoundaryWhenTheTimerHasNotFired(t *testing.T) {
	const hold = time.Hour
	for _, tt := range []struct {
		held time.Duration
		want []fired
	}{
		{hold - time.Millisecond, []fired{{GesturePressEnd, 0, 0}}},
		// Held at the threshold and past it: the boundary belongs to the hold.
		{hold, []fired{{GestureLongStart, 0, 0}, {GestureLongEnd, 0, hold}}},
		{2 * hold, []fired{{GestureLongStart, 0, 0}, {GestureLongEnd, 0, 2 * hold}}},
	} {
		m, got := collectorWith(testGap, hold)
		m.Down()
		m.Up(tt.held)
		settle()

		if !reflect.DeepEqual(got(), tt.want) {
			t.Errorf("a press held %v reported %v, want %v", tt.held, got(), tt.want)
		}
	}
}

// And the backstop ends a run in front of it, the way the timer does.
func TestTheBackstopEndsTheRunInFrontOfIt(t *testing.T) {
	const hold = time.Hour
	m, got := collectorWith(testGap, hold)
	m.tap()
	m.tap()
	m.Down()
	m.Up(hold)
	settle()

	want := []fired{{GestureMultiEnd, 2, 0}, {GestureLongStart, 0, 0}, {GestureLongEnd, 0, hold}}
	if !reflect.DeepEqual(got(), want) {
		t.Errorf("two presses then a held release reported %v, want %v", got(), want)
	}
}

// A hold ends the run in front of it at the threshold, not at the release, and
// the run is reported first: the far end runs them in the order they arrive.
func TestAHoldEndsTheRunInFrontOfIt(t *testing.T) {
	m, got := collector()
	m.tap()
	m.tap()
	m.Down()
	time.Sleep(3 * testHold)
	m.Up(time.Second)
	settle()

	want := []fired{
		{GestureMultiEnd, 2, 0},
		{GestureLongStart, 0, 0},
		{GestureLongEnd, 0, time.Second},
	}
	if !reflect.DeepEqual(got(), want) {
		t.Errorf("two presses then a hold reported %v, want %v", got(), want)
	}
}

// A hold with nothing before it is one gesture. The empty run in front is not
// one, and reporting it would invent a press.
func TestAHoldOnItsOwnReportsNoRun(t *testing.T) {
	m, got := collector()
	m.Down()
	time.Sleep(3 * testHold)
	m.Up(200 * time.Millisecond)
	settle()

	want := []fired{{GestureLongStart, 0, 0}, {GestureLongEnd, 0, 200 * time.Millisecond}}
	if !reflect.DeepEqual(got(), want) {
		t.Errorf("a hold with nothing before it reported %v, want %v", got(), want)
	}
}

// A run cannot close under a live key, so a press arriving inside the gap and
// then held reports the run at the threshold instead.
func TestARunDoesNotCloseWhileAKeyIsDown(t *testing.T) {
	m, got := collector()
	m.tap()
	m.Down()
	time.Sleep(3 * testGap)

	// The gap has passed twice and the run is still open, because the key is
	// down. Without that this would already report press_end.
	if n := len(got()); n != 0 {
		t.Fatalf("a run closed under a key that was still down: %v", got())
	}
	time.Sleep(3 * testHold)
	m.Up(time.Second)
	settle()

	want := []fired{
		{GesturePressEnd, 0, 0},
		{GestureLongStart, 0, 0},
		{GestureLongEnd, 0, time.Second},
	}
	if !reflect.DeepEqual(got(), want) {
		t.Errorf("a press then a hold reported %v, want %v", got(), want)
	}
}

// A timer that has fired cannot be stopped, so a generation tells it its moment
// passed. Both call the expiry directly with the generation the timer captured:
// the window -- fired, and waiting on the lock a key event holds -- is not one a
// test can wait for.

// The hold timer fires as the key comes up. It would report a key held that is
// no longer down.
func TestAHoldTimerThatLostItsKeyReportsNothing(t *testing.T) {
	m, got := collector()
	m.Down()
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()

	m.Up(time.Millisecond)
	m.held(gen)
	settle()

	if want := []fired{{GesturePressEnd, 0, 0}}; !reflect.DeepEqual(got(), want) {
		t.Errorf("a hold timer that fired after its key came up reported %v, want %v", got(), want)
	}
}

// The gap timer fires as the next key goes down. It would close a run under a
// press still in progress, which then reports again.
func TestAGapTimerThatLostItsRunReportsNothing(t *testing.T) {
	m, got := collector()
	m.tap()
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()

	m.Down()
	m.closed(gen)
	if n := len(got()); n != 0 {
		t.Fatalf("a gap timer that fired under a live key reported %v, want nothing", got())
	}
	m.Up(time.Millisecond)
	settle()

	// One run of two, not a press_end for the run it closed early and another
	// for the remainder.
	if want := []fired{{GestureMultiEnd, 2, 0}}; !reflect.DeepEqual(got(), want) {
		t.Errorf("a press either side of a stale gap timer reported %v, want %v", got(), want)
	}
}
