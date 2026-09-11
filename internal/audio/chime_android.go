package audio

/*
#cgo LDFLAGS: -lOpenSLES
#include <stdlib.h>
#include "audio.h"
*/
import "C"

import (
	"errors"
	"sync"
	"sync/atomic"
	"unsafe"
)

var open atomic.Bool

type Chime struct {
	mu     sync.Mutex
	pcm    unsafe.Pointer
	closed bool
}

func NewChime() (*Chime, error) {
	if !open.CompareAndSwap(false, true) {
		return nil, errors.New("audio: a chime is already open, and the player is process-wide")
	}
	clip := chimePCM()
	pcm := C.CBytes(clip)
	rc := C.audio_init((*C.uchar)(pcm), C.size_t(len(clip)), chimeRate, chimeChannels)
	if rc != 0 {
		C.free(pcm)
		open.Store(false)
		return nil, errors.New("audio_init: OpenSL ES would not start")
	}
	return &Chime{pcm: pcm}, nil
}

func (c *Chime) Play() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("audio: play after close")
	}
	if C.audio_play() != 0 {
		return errors.New("audio_play: the player would not start")
	}
	return nil
}

func (c *Chime) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	C.audio_close()
	C.free(c.pcm)
	c.pcm = nil
	open.Store(false)
}
