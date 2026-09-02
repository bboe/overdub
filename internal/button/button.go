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

func (i *Interceptor) Run(onPress func(held time.Duration)) error {
	return i.route(i.node, i.clone.Emit, onPress)
}

func (i *Interceptor) route(r io.Reader, emit func(uint16, int32) error, onPress func(held time.Duration)) error {
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
