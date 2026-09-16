package audio

/*
#cgo LDFLAGS: -lOpenSLES
#include "audio.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

var open atomic.Bool

type Chime struct {
	mu     sync.Mutex
	clip   []byte
	closed bool
}

func NewChime() (*Chime, error) {
	if !open.CompareAndSwap(false, true) {
		return nil, errors.New("audio: a chime is already open, and the player is process-wide")
	}
	clip := chimePCM()
	if capacity := int(C.audio_capacity()); len(clip) > capacity {
		open.Store(false)
		return nil, fmt.Errorf("audio: the chime is %d bytes against a %d byte queue,"+
			" and it is played in one go", len(clip), capacity)
	}
	if C.audio_open(ChimeRate, ChimeChannels) != 0 {
		open.Store(false)
		return nil, errors.New("audio_open: OpenSL ES would not start")
	}
	return &Chime{clip: clip}, nil
}

func (c *Chime) Play() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("audio: play after close")
	}
	if C.audio_reset() != 0 {
		return errors.New("audio_reset: the queue would not clear")
	}
	if err := feed(c.clip, writePCM); err != nil {
		return err
	}
	if C.audio_start() != 0 {
		return errors.New("audio_start: the player would not start")
	}
	return nil
}

func writePCM(pcm []byte) (int, error) {
	n := C.audio_write((*C.uchar)(unsafe.Pointer(&pcm[0])), C.size_t(len(pcm)))
	if n < 0 {
		return 0, errors.New("audio_write: the player would not take the samples")
	}
	return int(n), nil
}

func (c *Chime) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	C.audio_close()
	open.Store(false)
}
