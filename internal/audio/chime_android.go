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
	stream atomic.Pointer[Stream]
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

func (c *Chime) OpenStream(say func(string, ...any)) (*Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("audio: a stream after close")
	}
	if held := c.stream.Load(); held != nil {
		if !held.Spent() {
			return nil, errors.New("audio: a stream is already open, and this player" +
				" holds one")
		}
		held.Close()
		c.stream.CompareAndSwap(held, nil)
	}
	s := &Stream{say: say}
	s.closer = func() { c.stream.CompareAndSwap(s, nil) }
	if !c.stream.CompareAndSwap(nil, s) {
		return nil, errors.New("audio: a stream is already open, and this player holds one")
	}
	c.mix.add(s)
	return s, nil
}

func (c *Chime) live() *Stream { return c.stream.Load() }

func (c *Chime) look() {
	s := c.live()
	if s == nil {
		return
	}
	if s.Spent() {
		s.Close()
		return
	}
	if !s.wants() {
		return
	}
	p, err := c.ahead()
	if err != nil {
		s.blind(err)
		return
	}
	s.observe(p)
}

func (c *Chime) write() {
	defer close(c.done)
	block := make([]int16, BlockFrames)
	buf := make([]byte, BlockBytes)
	for {
		c.look()
		if !c.mix.next(block) {
			c.look()
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

func (c *Chime) ahead() (Point, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Point{}, errors.New("audio: asked after close")
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
	return point(int64(C.audio_pending()), st, at)
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
	if s := c.stream.Load(); s != nil {
		s.Close()
	}
	C.audio_close()
	open.Store(false)
}
