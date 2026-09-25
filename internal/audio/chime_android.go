package audio

/*
#cgo LDFLAGS: -lOpenSLES
#include "audio.h"
*/
import "C"

import (
	"errors"
	"fmt"
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
	rate   int
	clips  map[int]*clip
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
	clips := map[int]*clip{}
	for _, rate := range Rates() {
		clips[rate] = &clip{pcm: decode(chimePCM(rate))}
	}
	c := &Chime{
		mix:   newMixer(),
		rate:  ChimeRate,
		clips: clips,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
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
	c.mix.start(c.clips[c.rate])
	return nil
}

func (c *Chime) OpenStream(rate int, say func(string, ...any)) (*Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("audio: a stream after close")
	}
	if c.clips[rate] == nil {
		return nil, fmt.Errorf("audio: this player does not open at %d Hz", rate)
	}
	if held := c.stream.Load(); held != nil {
		if !held.Spent() {
			return nil, errors.New("audio: a stream is already open, and this player" +
				" holds one")
		}
		held.Close()
		c.stream.CompareAndSwap(held, nil)
	}
	s := &Stream{rate: rate, say: say}
	s.closer = func() { c.stream.CompareAndSwap(s, nil) }
	if !c.stream.CompareAndSwap(nil, s) {
		return nil, errors.New("audio: a stream is already open, and this player holds one")
	}
	if rate == c.rate {
		c.mix.add(s)
	} else {
		c.mix.wake()
	}
	return s, nil
}

func (c *Chime) retune() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.live()
	if c.closed || s == nil || s.rate == c.rate || s.Spent() {
		return true
	}
	c.mix.remove(c.clips[c.rate])
	C.audio_close()
	if C.audio_open(C.int(s.rate), ChimeChannels) != 0 || C.audio_start() != 0 {
		log.Printf("audio: the player would not open again at %d Hz; the Dot is silent until"+
			" the daemon restarts", s.rate)
		return false
	}
	c.rate = s.rate
	c.mix.add(s)
	return true
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
	block := make([]int16, BlockSamples)
	buf := make([]byte, BlockBytes)
	for {
		c.look()
		if !c.retune() {
			return
		}
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
	st, err := outputStatus(socketsPath, statusPath)
	if err != nil {
		return Point{}, err
	}
	at := time.Now()
	return point(int64(C.audio_pending()), st, at, c.rate)
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
