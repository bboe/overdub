package button

import (
	"io"
	"os"
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
	if err := evdev.Grab(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &VolumeGuard{node: file}, nil
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
