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

// The sound is the daemon's own, played through the daemon's own OpenSL ES
// player. Handing a URL to Alexa's SpeechSynthesizer was the previous route and
// cost 691ms against this one's 33, needed a loopback http server for the life
// of the daemon, and only ever worked while her stack was alive.

// There can be one. The player behind this is a set of C globals, so a second
// Chime would overwrite the first's handles, and Close on either would then
// destroy the other's player and free PCM its queue still points into. Refused
// rather than documented, because a convention that nothing checks is one a
// later caller cannot see.
var open atomic.Bool

// Chime plays one sound, and is the only thing this package does.
//
// The player behind it is process-wide, so the lock here serialises every call
// into it rather than merely the calls on one Chime. Two presses a few
// milliseconds apart would otherwise interleave a stop, a clear and an enqueue
// with each other.
type Chime struct {
	mu     sync.Mutex
	pcm    unsafe.Pointer
	closed bool
}

// NewChime builds the player and holds it open. Building it per press cost
// 333ms of process startup and dynamic linking, which is the whole reason this
// lives in the daemon rather than in a helper.
func NewChime() (*Chime, error) {
	if !open.CompareAndSwap(false, true) {
		return nil, errors.New("audio: a chime is already open, and the player is process-wide")
	}
	clip := chimePCM()
	pcm := C.CBytes(clip)
	rc := C.audio_init((*C.uchar)(pcm), C.size_t(len(clip)), chimeRate, chimeChannels)
	if rc != 0 {
		// audio_init unwinds itself, so the pointer it was handed is ours to
		// free again and the slot is free for a later attempt.
		C.free(pcm)
		open.Store(false)
		return nil, errors.New("audio_init: OpenSL ES would not start")
	}
	return &Chime{pcm: pcm}, nil
}

// Play returns as soon as the clip is queued; the sound outlives the call.
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

// Close tears the player down before the PCM it is reading from goes away, and
// is safe to call twice: the queue holds a pointer into that memory rather than
// a copy, so freeing it a second time, or playing after it, reads freed memory.
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
