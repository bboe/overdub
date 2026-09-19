package button

import (
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
