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

func (m *MultiPress) tap() {
	m.Down()
	m.Up(time.Millisecond)
}

func settle() { time.Sleep(4 * testGap) }

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

func TestTheReleaseDecidesTheBoundaryWhenTheTimerHasNotFired(t *testing.T) {
	const hold = time.Hour
	for _, tt := range []struct {
		held time.Duration
		want []fired
	}{
		{hold - time.Millisecond, []fired{{GesturePressEnd, 0, 0}}},
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

func TestARunDoesNotCloseWhileAKeyIsDown(t *testing.T) {
	m, got := collector()
	m.tap()
	m.Down()
	time.Sleep(3 * testGap)

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

	if want := []fired{{GestureMultiEnd, 2, 0}}; !reflect.DeepEqual(got(), want) {
		t.Errorf("a press either side of a stale gap timer reported %v, want %v", got(), want)
	}
}
