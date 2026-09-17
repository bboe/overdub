package audio

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"
)

const (
	frameBytes = ChimeChannels * 2

	streamAhead  = 30 * time.Second
	streamHold   = 4 * ChimeRate
	streamChunks = streamHold / BlockFrames

	anchorSettle = 100 * time.Millisecond
	anchorTake   = 5

	anchorSlip = 50 * time.Millisecond
	slipRuns   = 3

	observeEvery = 10
	blindAfter   = 100

	drainWait = 5 * time.Second
)

var (
	errStreamClosed = errors.New("audio: the stream is closed")
	errStreamDone   = errors.New("audio: the stream has been ended and is playing out")
)

func frameTime(n int64) time.Duration {
	return time.Duration(n/ChimeRate)*time.Second +
		time.Duration(n%ChimeRate)*time.Second/ChimeRate
}

func frameCount(d time.Duration) int64 { return int64(d) * ChimeRate / int64(time.Second) }

type queued struct {
	at  time.Time
	pcm []int16
	off int
}

type Stream struct {
	say    func(string, ...any)
	closer func()

	mu     sync.Mutex
	queue  []queued
	held   int64
	index  int64
	blocks int64
	first  time.Time
	done   bool
	doneAt time.Time
	spent  bool
	closed bool

	anchored    bool
	origin      time.Time
	originIndex int64
	ref         int64
	candidates  []time.Time

	asked     int64
	unread    int
	outside   int
	saidBlind bool

	placed  int64
	silence int64
	late    int64
	slips   int
}

func (s *Stream) report(format string, args ...any) {
	if s.say != nil {
		s.say(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (s *Stream) Write(at time.Time, pcm []byte) error {
	if len(pcm)%frameBytes != 0 {
		return fmt.Errorf("audio: %d bytes is not a whole number of %d-byte frames",
			len(pcm), frameBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.spent {
		return errStreamClosed
	}
	if s.done {
		return errStreamDone
	}
	if due := time.Until(at); due > streamAhead || due < -streamAhead {
		return fmt.Errorf("audio: audio due %s from now, which is further off than the %s"+
			" this player will hold", due.Round(time.Millisecond), streamAhead)
	}
	frames := int64(len(pcm) / frameBytes)
	if s.held+frames > streamHold {
		return fmt.Errorf("audio: %s already buffered, so another %d frames passes the %s"+
			" this player holds", frameTime(s.held), frames, frameTime(streamHold))
	}
	if len(s.queue) >= streamChunks {
		return fmt.Errorf("audio: %d chunks are already queued, so another one passes the"+
			" %d this player holds", len(s.queue), streamChunks)
	}
	i := len(s.queue)
	for i > 0 && s.queue[i-1].at.After(at) && s.queue[i-1].off == 0 {
		i--
	}
	s.queue = slices.Insert(s.queue, i, queued{at: at, pcm: decode(pcm)})
	s.held += frames
	return nil
}

func (s *Stream) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.spent || s.done {
		return
	}
	s.done, s.doneAt = true, time.Now()
}

func (s *Stream) Resume() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.spent {
		return false
	}
	s.done = false
	return true
}

func (s *Stream) Placed() (audio, silence int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.placed, s.silence
}

func (s *Stream) Spent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent || s.closed
}

func (s *Stream) drained() bool {
	return s.done && (len(s.queue) == 0 || time.Since(s.doneAt) >= drainWait)
}

func (s *Stream) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue, s.held = nil, 0
}

func (s *Stream) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	placed, silence, late, slips := s.placed, s.silence, s.late, s.slips
	anchored, held := s.anchored, s.held
	s.mu.Unlock()

	if anchored {
		s.report("audio: the stream placed %s of audio against %s of silence, dropped %s"+
			" that arrived late, and was placed again %d times", frameTime(placed),
			frameTime(silence), frameTime(late), slips)
	} else {
		s.report("audio: the stream never learned where the player had reached, so it"+
			" played %s of silence and threw away the %s it was holding",
			frameTime(silence), frameTime(held))
	}
	if s.closer != nil {
		s.closer()
	}
}

func (s *Stream) read(block []int16) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.spent {
		return 0, false
	}
	if s.drained() {
		s.spent = true
		return 0, false
	}
	if s.first.IsZero() {
		s.first = time.Now()
	}
	clear(block)
	filled := 0
	if s.anchored {
		filled = s.fill(block)
	}
	s.placed += int64(filled)
	s.silence += int64(len(block) - filled)
	s.index += int64(len(block))
	s.blocks++
	return len(block), true
}

func (s *Stream) fill(block []int16) int {
	pos, filled := 0, 0
	for pos < len(block) && len(s.queue) > 0 {
		c := &s.queue[0]
		start := s.frameAt(c.at) + int64(c.off)
		left := int64(len(c.pcm) - c.off)
		want := s.index + int64(pos)
		switch {
		case start+left <= want:
			s.drop(left)
		case start < want:
			s.skip(want - start)
		case start > want:
			pos += int(min(start-want, int64(len(block)-pos)))
		default:
			n := copy(block[pos:], c.pcm[c.off:])
			c.off += n
			s.held -= int64(n)
			pos, filled = pos+n, filled+n
			if c.off == len(c.pcm) {
				s.queue = s.queue[1:]
			}
		}
	}
	return filled
}

func (s *Stream) drop(frames int64) {
	s.late += frames
	s.held -= frames
	s.queue = s.queue[1:]
}

func (s *Stream) skip(frames int64) {
	s.queue[0].off += int(frames)
	s.late += frames
	s.held -= frames
}

func (s *Stream) frameAt(when time.Time) int64 {
	return s.originIndex + frameCount(when.Sub(s.origin))
}

func (s *Stream) wants() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.first.IsZero() || time.Since(s.first) < anchorSettle {
		return false
	}
	return !s.anchored || s.blocks-s.asked >= observeEvery
}

func (s *Stream) blind(err error) {
	s.mu.Lock()
	s.asked = s.blocks
	s.unread++
	say := s.unread == blindAfter && !s.saidBlind
	s.saidBlind = s.saidBlind || say
	s.mu.Unlock()
	if say {
		s.report("audio: the player has not said where its audio has reached for %d"+
			" readings, so nothing can be placed and the stream is silent: %v",
			blindAfter, err)
	}
}

func (s *Stream) observe(p Point) {
	depth, slip, again := s.place(p)
	switch {
	case depth > 0:
		s.report("audio: the player is %s ahead of the speaker, so the stream is placed"+
			" against that", depth.Round(time.Millisecond))
	case again:
		s.report("audio: the stream was %s out of step with the player, so it was placed"+
			" again", slip.Round(time.Millisecond))
	}
}

func (s *Stream) place(p Point) (depth, slip time.Duration, again bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = s.blocks
	s.unread = 0

	due := p.At.Add(frameTime(p.Ahead))
	if !s.anchored {
		if len(s.candidates) == 0 {
			s.ref = s.index
		}
		s.candidates = append(s.candidates, due.Add(-frameTime(s.index-s.ref)))
		if len(s.candidates) < anchorTake {
			return 0, 0, false
		}
		slices.SortFunc(s.candidates, time.Time.Compare)
		s.origin, s.originIndex = s.candidates[len(s.candidates)/2], s.ref
		s.candidates, s.anchored = nil, true
		return due.Sub(p.At), 0, false
	}
	slip = due.Sub(s.origin.Add(frameTime(s.index - s.originIndex)))
	if slip <= anchorSlip && slip >= -anchorSlip {
		s.outside = 0
		return 0, 0, false
	}
	if s.outside++; s.outside < slipRuns {
		return 0, 0, false
	}
	s.origin, s.originIndex = due, s.index
	s.outside, s.slips = 0, s.slips+1
	return 0, slip, true
}
