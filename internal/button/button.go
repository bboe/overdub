// Package button owns the exclusive grab on the action button, and the uinput
// device that stands in for the real one while it is held.
package button

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bboe/overdub/internal/evdev"
)

type Interceptor struct {
	node    *os.File
	clone   *evdev.Uinput
	consume uint16
	once    sync.Once

	// Whether the consumed key is being handed to the clone rather than acted
	// on. Stated this way round so that the zero value is a captured button:
	// a construction path that never sets it keeps the key, which is what this
	// daemon is for, rather than quietly giving it back. Read by the read loop
	// and written from whichever goroutine calls SetCaptured.
	passing atomic.Bool
}

func Open(path string, consume uint16, uiName string, wait time.Duration) (*Interceptor, error) {
	if err := waitForNode(path, wait); err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var uinput *evdev.Uinput
	started := false
	defer func() {
		if started {
			return
		}
		if uinput != nil {
			_ = uinput.Close()
		}
		_ = file.Close()
	}()

	keys, err := evdev.DeviceKeys(file)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s declares no keycodes", path)
	}
	// Fatal rather than defaulted, for the reason a wrong key bitmap is: an id
	// this end made up is one Android resolves a different keylayout from, and
	// it would go wrong silently.
	id, err := evdev.DeviceID(file)
	if err != nil {
		return nil, err
	}
	log.Printf("cloning %d keycodes from %s (bus %#04x vendor %#04x product %#04x version %#04x)",
		len(keys), path, id.Bus, id.Vendor, id.Product, id.Version)

	uinput, err = evdev.NewUinput(uiName, id, keys)
	if err != nil {
		return nil, fmt.Errorf("uinput: %w (is CONFIG_UINPUT present, and are you root?)", err)
	}

	if err := evdev.Grab(file, true); err != nil {
		return nil, err
	}

	started = true
	return &Interceptor{node: file, clone: uinput, consume: consume}, nil
}

func (i *Interceptor) Captured() bool { return !i.passing.Load() }

// Hands the key to Alexa and takes it back. The grab is untouched either way --
// it is what stops the real node reaching EventHub at all, so releasing it
// would leave the clone echoing a live original and land every key twice.
// Letting the key go means re-emitting it through the clone instead.
//
// It takes effect at the next press rather than at the next event, so a toggle
// arriving mid-press cannot split one between the two paths.
func (i *Interceptor) SetCaptured(on bool) { i.passing.Store(!on) }

// The caller's signal handler and its defer can both reach this, so it has to
// be idempotent. A second pass would be inert rather than harmful -- an
// os.File remembers that it is closed -- but once is what the caller expects.
func (i *Interceptor) Close() {
	i.once.Do(func() {
		_ = i.clone.Close()
		_ = evdev.Grab(i.node, false)
		_ = i.node.Close()
	})
}

// The key is reported twice, because the two halves answer different questions.
// onDown is the acknowledgement and has to sound before anybody knows what the
// press will be; onPress carries how long it lasted.
//
// Both run on the read loop, which mute passes through, so neither may block for
// long. onPress may wait: MultiPress orders its reports under one lock, and a
// timer goroutine can hold it across the caller's own callback. That callback
// therefore bounds how long mute can be delayed, whichever goroutine runs it.
func (i *Interceptor) Run(onDown func(), onPress func(held time.Duration)) error {
	return i.route(i.node, i.clone.Emit, onDown, onPress)
}

func (i *Interceptor) route(r io.Reader, emit func(uint16, int32) error, onDown func(), onPress func(held time.Duration)) error {
	var pressedAt time.Time
	consuming := false
	buf := make([]byte, evdev.EventSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		event := evdev.Unmarshal(buf)

		if event.Type == evdev.EvKey && event.Code == i.consume {
			switch event.Value {
			case evdev.KeyPress:
				// Latched here and held to the release, so a toggle inside one
				// press cannot send its halves different ways: a release
				// Android was never given a press for, or a press it is never
				// told ended.
				consuming, pressedAt = i.Captured(), time.Now()
				if consuming {
					onDown()
				}
			case evdev.KeyRelease:
				// No press of ours: the key was down before the grab took it,
				// so neither path has a whole event to act on.
				if pressedAt.IsZero() {
					continue
				}
				held := time.Since(pressedAt)
				pressedAt = time.Time{}
				if consuming {
					onPress(held)
				}
			default:
				// Autorepeat, which belongs to the press in flight if there is
				// one and to nothing at all if there is not.
				if pressedAt.IsZero() {
					continue
				}
			}
			if consuming {
				continue
			}
		}

		if event.Type == evdev.EvKey {
			if err := emit(event.Code, event.Value); err != nil {
				return fmt.Errorf("passthrough failed for %d: %w", event.Code, err)
			}
		}
	}
}

func waitForNode(path string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%s never appeared", path)
		}
		time.Sleep(min(remaining, time.Second))
	}
}

// What the button did. Named for the gesture rather than the ESPHome type it
// becomes, so the caller still decides what a press means.
type Gesture int

const (
	// One press, alone: reported once the gap has passed without a second.
	GesturePressEnd Gesture = iota
	// The run, once the gap has closed it.
	GestureMultiEnd
	// The key is still down and has passed the hold threshold.
	GestureLongStart
	// It came back up.
	GestureLongEnd
)

func (g Gesture) String() string {
	switch g {
	case GesturePressEnd:
		return "press_end"
	case GestureMultiEnd:
		return "multi_press_end"
	case GestureLongStart:
		return "long_press_start"
	case GestureLongEnd:
		return "long_press_end"
	}
	return "unknown"
}

// Turns the key into gestures. Driven by the key rather than by the release,
// because a hold must be reported while the key is still down.
//
// Two timers run. The hold timer is armed at each key-down and reports if the
// key is still down when it fires. The gap timer is armed at each release and
// closes the run. Every key event invalidates both, so a run cannot close under
// a live key and a hold cannot be reported for one that came up.
type MultiPress struct {
	gap      time.Duration
	holdTime time.Duration

	// count is the run's, and zero once a hold has taken it. holdFor is the
	// hold's, and zero elsewhere: a run has no single duration.
	fire func(g Gesture, count int, holdFor time.Duration)

	// Held across a whole report, so gestures reach the caller in order. Taken
	// before mu by everything that reports.
	out sync.Mutex

	mu    sync.Mutex
	count int
	// The press in flight was reported as a hold, so its release ends it rather
	// than adding to the run.
	holding   bool
	gapTimer  *time.Timer
	holdTimer *time.Timer

	// Bumped by every key event. A timer that has fired cannot be stopped, so
	// one waiting on out finds its generation stale and reports nothing --
	// otherwise a run somebody took, or a hold for a key that came up.
	gen uint64
}

func NewMultiPress(gap, holdTime time.Duration, fire func(g Gesture, count int, holdFor time.Duration)) *MultiPress {
	return &MultiPress{gap: gap, holdTime: holdTime, fire: fire}
}

// The key went down. Reports nothing: nothing is known yet.
func (m *MultiPress) Down() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	stop(&m.gapTimer)
	gen := m.gen
	m.holdTimer = time.AfterFunc(m.holdTime, func() { m.held(gen) })
}

// The key came up, after being down for held.
func (m *MultiPress) Up(held time.Duration) {
	m.out.Lock()
	defer m.out.Unlock()

	m.mu.Lock()
	// Bumped before the stop, because a timer that already fired cannot be
	// stopped and this is what tells it the key came up.
	m.gen++
	stop(&m.holdTimer)
	if m.holding {
		m.holding = false
		m.mu.Unlock()
		m.fire(GestureLongEnd, 0, held)
		return
	}
	// The timer decides a hold, and it can be late: AfterFunc says no more than
	// "not before", and this runs on one busy core. The release carries the
	// duration, so it is the backstop rather than a second opinion -- without it
	// a key genuinely held past the threshold is reported as a press whenever
	// the timer loses the race to Up.
	if held >= m.holdTime {
		n := m.count
		m.count = 0
		m.mu.Unlock()
		m.report(n)
		m.fire(GestureLongStart, 0, 0)
		m.fire(GestureLongEnd, 0, held)
		return
	}
	m.count++
	gen := m.gen
	m.gapTimer = time.AfterFunc(m.gap, func() { m.closed(gen) })
	m.mu.Unlock()
}

// The hold threshold passed with the key still down.
func (m *MultiPress) held(gen uint64) {
	m.out.Lock()
	defer m.out.Unlock()

	m.mu.Lock()
	if gen != m.gen {
		m.mu.Unlock()
		return
	}
	n := m.count
	m.count = 0
	m.holding = true
	m.holdTimer = nil
	m.mu.Unlock()

	// The run ended here rather than at the release, and is reported first:
	// separate automations at the far end run in the order they arrive.
	m.report(n)
	m.fire(GestureLongStart, 0, 0)
}

// The gap passed without another press.
func (m *MultiPress) closed(gen uint64) {
	m.out.Lock()
	defer m.out.Unlock()

	m.mu.Lock()
	if gen != m.gen {
		m.mu.Unlock()
		return
	}
	n := m.count
	m.count = 0
	m.gapTimer = nil
	m.mu.Unlock()

	m.report(n)
}

// A run of no presses is not a gesture. It is what a hold with nothing before it
// leaves behind, and reporting it would invent a press.
func (m *MultiPress) report(count int) {
	switch {
	case count <= 0:
		return
	case count == 1:
		m.fire(GesturePressEnd, 0, 0)
	default:
		m.fire(GestureMultiEnd, count, 0)
	}
}

func stop(t **time.Timer) {
	if *t != nil {
		(*t).Stop()
		*t = nil
	}
}
