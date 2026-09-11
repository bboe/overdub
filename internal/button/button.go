// Package button owns the exclusive grab on the action button, and the uinput
// device that stands in for the real one while it is held.
package button

import (
	"errors"
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

	emitMu sync.Mutex

	watched map[uint16]*watch
}

type watch struct {
	mode atomic.Int32

	pressedAt time.Time
	latched   Mode
}

type Mode int32

const (
	ModeIntercept Mode = iota
	ModeMonitor
	ModePassThrough
)

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

func (i *Interceptor) Mode(code uint16) Mode {
	w, ok := i.watched[code]
	if !ok {
		return ModePassThrough
	}
	return Mode(w.mode.Load())
}

func (i *Interceptor) SetMode(code uint16, m Mode) {
	if w, ok := i.watched[code]; ok {
		w.mode.Store(int32(m))
	}
}

func (i *Interceptor) Close() {
	i.once.Do(func() {
		_ = i.clone.Close()
		_ = evdev.Grab(i.node, false)
		_ = i.node.Close()
	})
}

func (i *Interceptor) Run(onDown func(uint16, Mode), onPress func(uint16, Mode, time.Duration)) error {
	return i.route(i.node, i.emit, onDown, onPress)
}

func (i *Interceptor) emit(code uint16, value int32) error {
	i.emitMu.Lock()
	defer i.emitMu.Unlock()

	return i.clone.Emit(code, value)
}

func (i *Interceptor) Press(code uint16) error {
	i.emitMu.Lock()
	defer i.emitMu.Unlock()

	return press(i.clone.Emit, code)
}

var ErrKeyStuck = errors.New("the key-up did not reach the clone")

func press(emit func(uint16, int32) error, code uint16) error {
	if err := emit(code, 1); err != nil {
		return fmt.Errorf("%w: %w", ErrKeyStuck, err)
	}
	if err := emit(code, 0); err != nil {
		return fmt.Errorf("%w: %w", ErrKeyStuck, err)
	}
	return nil
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
				w.latched, w.pressedAt = i.Mode(event.Code), time.Now()
				if w.latched != ModePassThrough {
					onDown(event.Code, w.latched)
				}
			case evdev.KeyRelease:
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
				if w.pressedAt.IsZero() {
					continue
				}
			}
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

type Gesture int

const (
	GesturePressEnd Gesture = iota
	GestureMultiEnd
	GestureLongStart
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

type MultiPress struct {
	gap      time.Duration
	holdTime time.Duration

	fire func(g Gesture, count int, holdFor time.Duration)

	out sync.Mutex

	mu        sync.Mutex
	count     int
	holding   bool
	gapTimer  *time.Timer
	holdTimer *time.Timer

	gen uint64
}

func NewMultiPress(gap, holdTime time.Duration, fire func(g Gesture, count int, holdFor time.Duration)) *MultiPress {
	return &MultiPress{gap: gap, holdTime: holdTime, fire: fire}
}

func (m *MultiPress) Down() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	stop(&m.gapTimer)
	gen := m.gen
	m.holdTimer = time.AfterFunc(m.holdTime, func() { m.held(gen) })
}

func (m *MultiPress) Up(held time.Duration) {
	m.out.Lock()
	defer m.out.Unlock()

	m.mu.Lock()
	m.gen++
	stop(&m.holdTimer)
	if m.holding {
		m.holding = false
		m.mu.Unlock()
		m.fire(GestureLongEnd, 0, held)
		return
	}
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

	m.report(n)
	m.fire(GestureLongStart, 0, 0)
}

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
