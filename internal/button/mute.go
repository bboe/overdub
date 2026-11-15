package button

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bboe/overdub/internal/evdev"
)

const (
	KeyVolumeDown = 114
	KeyVolumeUp   = 115
)

type VolumeGuard struct {
	node *os.File
	once sync.Once
}

func GuardVolume(path string, wait time.Duration) (*VolumeGuard, error) {
	if err := waitForNode(path, wait); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := volumeKeysOn(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := evdev.Grab(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &VolumeGuard{node: file}, nil
}

func volumeKeysOn(file *os.File) error {
	keys, err := evdev.DeviceKeys(file)
	if err != nil {
		return err
	}
	for _, want := range []uint16{KeyVolumeUp, KeyVolumeDown} {
		if !slices.Contains(keys, want) {
			return fmt.Errorf("%s declares no keycode %d", file.Name(), want)
		}
	}
	return nil
}

func (g *VolumeGuard) Wait() error { return waitForVolumeKey(g.node) }

func waitForVolumeKey(r io.Reader) error {
	buf := make([]byte, evdev.EventSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		event := evdev.Unmarshal(buf)
		if event.Type != evdev.EvKey || event.Value != evdev.KeyPress {
			continue
		}
		if event.Code == KeyVolumeUp || event.Code == KeyVolumeDown {
			return nil
		}
	}
}

func (g *VolumeGuard) Close() {
	g.once.Do(func() {
		_ = evdev.Grab(g.node, false)
		_ = g.node.Close()
	})
}

var ErrReleased = errors.New("the volume keys were released")

type HeldVolume struct {
	i        *Interceptor
	presses  chan struct{}
	released chan struct{}
	once     sync.Once
}

func (i *Interceptor) HoldVolume() (*HeldVolume, error) {
	if !i.CarriesVolume() {
		return nil, errors.New("this node does not carry both volume keys under a grab")
	}
	h := &HeldVolume{i: i, presses: make(chan struct{}, 1), released: make(chan struct{})}
	i.volMu.Lock()
	defer i.volMu.Unlock()
	if i.held != nil {
		return nil, errors.New("the volume keys are already held")
	}
	i.held = h
	return h, nil
}

func (h *HeldVolume) Wait() error {
	select {
	case <-h.presses:
		return nil
	case <-h.released:
		return ErrReleased
	}
}

func (h *HeldVolume) Close() {
	h.once.Do(func() {
		h.i.volMu.Lock()
		if h.i.held == h {
			h.i.held = nil
		}
		h.i.volMu.Unlock()
		close(h.released)
	})
}

func (i *Interceptor) swallowVolume(e evdev.Event) bool {
	if e.Type != evdev.EvKey || e.Code != KeyVolumeUp && e.Code != KeyVolumeDown {
		return false
	}
	i.volMu.Lock()
	defer i.volMu.Unlock()
	if i.held != nil && e.Value == evdev.KeyPress {
		if i.swallowing == nil {
			i.swallowing = map[uint16]bool{}
		}
		i.swallowing[e.Code] = true
		select {
		case i.held.presses <- struct{}{}:
		default:
		}
		return true
	}
	if i.held != nil && e.Value == evdev.KeyRepeat {
		return true
	}
	if !i.swallowing[e.Code] {
		return false
	}
	if e.Value == evdev.KeyRelease {
		delete(i.swallowing, e.Code)
	}
	return true
}
