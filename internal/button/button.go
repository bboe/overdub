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
	node  *os.File
	clone *evdev.Uinput
	once  sync.Once

	// The keys this end acts on, by keycode. Every other key the node carries
	// is re-emitted untouched, which is what the clone is for. Written once by
	// Open and only read afterwards, so the read loop needs no lock to find a
	// key -- and each watch carries its own mode, so two keys can be in
	// different modes at once.
	watched map[uint16]*watch
}

type watch struct {
	// Written from whichever goroutine calls SetMode, read by the read loop.
	mode atomic.Int32

	// The read loop's alone. A key is latched at its down and held to its up,
	// and each key has its own pair because one can be held while another is
	// pressed.
	pressedAt time.Time
	latched   Mode
}

// What the daemon does with a key it was given.
type Mode int32

const (
	// The key is consumed and reported. The zero value, so a construction path
	// that never mentions a mode keeps the key rather than quietly handing it
	// to Alexa, which is what this daemon is for.
	ModeIntercept Mode = iota
	// The key is re-emitted and reported: Alexa answers it as she always did,
	// and Home Assistant hears about it too.
	ModeMonitor
	// The key is re-emitted and nothing is reported. The daemon holds the grab
	// either way, so this is the clone doing what the real node would have.
	ModePassThrough
)

// A mode with no name here answers with none, rather than with the first one.
// The alternative is a mode added later that reports itself as intercept: the
// select would show a button held while it was being passed through, and
// nothing at either end would say so.
func (m Mode) String() string {
	switch m {
	case ModeIntercept:
		return "intercept"
	case ModeMonitor:
		return "monitor"
	case ModePassThrough:
		return "pass through"
	}
	return ""
}

// Opens the node, grabs it, and stands a clone in its place. start says which
// keycodes this end acts on and what it does with each: a key absent from it is
// re-emitted like any other. The modes are given here rather than set
// afterwards because the caller is the only thing that knows which key is which,
// and a key that spent its first moments in the wrong mode would be one Alexa
// answered or did not.
func Open(path, uiName string, wait time.Duration, start map[uint16]Mode) (*Interceptor, error) {
	if len(start) == 0 {
		return nil, fmt.Errorf("%s: no keycodes to act on", path)
	}
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
	i := &Interceptor{node: file, clone: uinput, watched: map[uint16]*watch{}}
	for code, mode := range start {
		w := &watch{}
		w.mode.Store(int32(mode))
		i.watched[code] = w
	}
	return i, nil
}

// The mode of a key this end acts on. A key it does not is ModePassThrough,
// which is what the clone does with it anyway.
func (i *Interceptor) Mode(code uint16) Mode {
	w, ok := i.watched[code]
	if !ok {
		return ModePassThrough
	}
	return Mode(w.mode.Load())
}

// The grab is untouched by any of the modes -- it is what stops the real node
// reaching EventHub at all, so releasing it would leave the clone echoing a
// live original and land every key twice. What a mode chooses is whether the
// clone re-emits the key, and whether the caller hears about it.
//
// It takes effect at the next press rather than at the next event, so a change
// arriving mid-press cannot split one between two paths.
func (i *Interceptor) SetMode(code uint16, m Mode) {
	if w, ok := i.watched[code]; ok {
		w.mode.Store(int32(m))
	}
}

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

// A key is reported twice, because the two halves answer different questions.
// onDown is the acknowledgement and has to sound before anybody knows what the
// press will be; onPress carries how long it lasted. Both are given the keycode
// and the mode latched at its down, so one caller can serve several keys and
// tell a key it owns from one Alexa is answering too. Neither runs in
// ModePassThrough, where reporting nothing is the whole of what the mode means.
//
// Both run on the read loop, which every key passes through, so neither may
// block for long. onPress may wait: MultiPress orders its reports under one
// lock, and a timer goroutine can hold it across the caller's own callback.
// That callback therefore bounds how long every other key can be delayed,
// whichever goroutine runs it.
//
// The report happens before the re-emission, so on a monitored key the wait
// above is spent before Android sees the release rather than after. Emitting
// first would fix that and costs a second pass over the event kinds in the
// hottest loop here; the wait is a log write and a lock this end holds
// briefly, so it is measured in milliseconds and this is left as it is
// deliberately rather than by oversight.
func (i *Interceptor) Run(onDown func(uint16, Mode), onPress func(uint16, Mode, time.Duration)) error {
	return i.route(i.node, i.clone.Emit, onDown, onPress)
}

func (i *Interceptor) route(r io.Reader, emit func(uint16, int32) error, onDown func(uint16, Mode), onPress func(uint16, Mode, time.Duration)) error {
	buf := make([]byte, evdev.EventSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		event := evdev.Unmarshal(buf)

		if w, ours := i.watched[event.Code]; ours && event.Type == evdev.EvKey {
			switch event.Value {
			case evdev.KeyPress:
				// Latched here and held to the release, so a change inside one
				// press cannot send its halves different ways: a release
				// Android was never given a press for, or a press it is never
				// told ended.
				w.latched, w.pressedAt = i.Mode(event.Code), time.Now()
				if w.latched != ModePassThrough {
					onDown(event.Code, w.latched)
				}
			case evdev.KeyRelease:
				// No press of ours: the key was down before the grab took it,
				// so there is nothing to report. Android took that key-down
				// from the real node and no release of ours reaches it, since
				// KeyDowns are tracked per device -- but the app layer does not
				// look at the device, so a key Android is allowed to see still
				// gets its release. The latch is stale here, since no press set
				// it, so the live mode decides.
				if w.pressedAt.IsZero() {
					w.latched = i.Mode(event.Code)
					break
				}
				held := time.Since(w.pressedAt)
				w.pressedAt = time.Time{}
				if w.latched != ModePassThrough {
					onPress(event.Code, w.latched, held)
				}
			default:
				// Autorepeat, which belongs to the press in flight if there is
				// one. One with no press is dropped rather than passed on like
				// the release above: EventHub reads any non-zero value as a
				// down, so emitting it puts a fresh key-down on the clone, and
				// a mode change before the release would then swallow the only
				// thing that could end it.
				if w.pressedAt.IsZero() {
					continue
				}
			}
			// Only intercept keeps the key. The other two hand it to the clone
			// like every other key, which is the route mute has always taken.
			if w.latched == ModeIntercept {
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
