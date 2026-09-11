package button

import (
	"sync"
	"time"

	"github.com/bboe/overdub/internal/evdev"
)

const (
	KeyVolumeDown = 114
	KeyVolumeUp   = 115

	volumeGap = 30 * time.Millisecond
)

type VolumeKeys struct {
	mu   sync.Mutex
	keys *evdev.Uinput
	gap  time.Duration
}

func NewVolumeKeys(name string) (*VolumeKeys, error) {
	keys, err := evdev.NewUinput(name, evdev.InputID{}, []uint16{KeyVolumeDown, KeyVolumeUp})
	if err != nil {
		return nil, err
	}
	return &VolumeKeys{keys: keys, gap: volumeGap}, nil
}

func (v *VolumeKeys) Step(up bool, n int) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	return steps(v.keys.Emit, v.gap, up, n)
}

func steps(emit func(uint16, int32) error, gap time.Duration, up bool, n int) error {
	code := uint16(KeyVolumeDown)
	if up {
		code = KeyVolumeUp
	}
	for i := 0; i < n; i++ {
		if err := press(emit, code); err != nil {
			return err
		}
		time.Sleep(gap)
	}
	return nil
}

func (v *VolumeKeys) Close() error { return v.keys.Close() }
