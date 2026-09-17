package audio

/*
#cgo LDFLAGS: -lOpenSLES
#include "audio.h"
*/
import "C"

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var open atomic.Bool

type Chime struct {
	mu     sync.Mutex
	mix    *mixer
	clip   *clip
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

func NewChime() (*Chime, error) {
	if !open.CompareAndSwap(false, true) {
		return nil, errors.New("audio: a chime is already open, and the player is process-wide")
	}
	if C.audio_open(ChimeRate, ChimeChannels) != 0 {
		open.Store(false)
		return nil, errors.New("audio_open: OpenSL ES would not start")
	}
	if C.audio_start() != 0 {
		C.audio_close()
		open.Store(false)
		return nil, errors.New("audio_start: the player would not start")
	}
	c := &Chime{
		mix:  newMixer(),
		clip: &clip{pcm: decode(chimePCM())},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go c.write()
	return c, nil
}

func (c *Chime) Play() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("audio: play after close")
	}
	c.mix.start(c.clip)
	return nil
}

func (c *Chime) write() {
	defer close(c.done)
	block := make([]int16, BlockFrames)
	buf := make([]byte, BlockBytes)
	for {
		if !c.mix.next(block) {
			select {
			case <-c.stop:
				return
			case <-c.mix.woke:
				continue
			}
		}
		encode(block, buf)
		if !c.push(buf) {
			return
		}
	}
}

func (c *Chime) push(buf []byte) bool {
	err := writeAll(buf, writeChunk, c.waiting)
	if err == nil {
		return true
	}
	if !errors.Is(err, errStopping) {
		log.Printf("audio: the writer stopped: %v; the Dot is silent until the daemon"+
			" restarts", err)
	}
	return false
}

func writeChunk(buf []byte) (int, error) {
	n := int(C.audio_write((*C.uchar)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))))
	if n < 0 {
		return 0, errors.New("audio_write would not take the block")
	}
	return n, nil
}

func (c *Chime) waiting() bool {
	select {
	case <-c.stop:
		return false
	case <-time.After(writeWait):
		return true
	}
}

func (c *Chime) Played() (Point, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Point{}, errors.New("audio: played after close")
	}
	if !c.mix.sounding() {
		return Point{}, errors.New("audio: nothing of ours is playing, so the queue the" +
			" output reports is somebody else's")
	}
	st, err := readStatus(statusPath)
	if err != nil {
		return Point{}, err
	}
	at := time.Now()
	return point(int64(C.audio_position()), st, at)
}

func (c *Chime) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.stop)
	c.mu.Unlock()

	<-c.done
	C.audio_close()
	open.Store(false)
}
