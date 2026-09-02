// Package button owns the exclusive grab on the action button, and the uinput
// device that stands in for the real one while it is held.
package button

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/bboe/overdub/internal/evdev"
)

type Interceptor struct {
	node    *os.File
	clone   *evdev.Uinput
	consume uint16
	once    sync.Once
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
	buf := make([]byte, evdev.EventSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		event := evdev.Unmarshal(buf)

		if event.Type == evdev.EvKey && event.Code == i.consume {
			switch event.Value {
			case evdev.KeyPress:
				pressedAt = time.Now()
			case evdev.KeyRelease:
				if pressedAt.IsZero() {
					continue
				}
				onPress(time.Since(pressedAt))
				pressedAt = time.Time{}
			}
			continue
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
