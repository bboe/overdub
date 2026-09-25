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
	streamHold   = 30 * time.Second
	streamChunks = 3000

	anchorSettle = 100 * time.Millisecond
	anchorTake   = 5

	anchorSlip = 50 * time.Millisecond
	slipRuns   = 3

	burstyFirst = 20
	burstyOver  = 128
	burstyBand  = 4 * time.Millisecond
	burstySlip  = 150 * time.Millisecond

	deadBand   = 5 * time.Millisecond
	softStep   = 21 * time.Microsecond
	easeApart  = 200
	snapAbove  = 50 * time.Millisecond
	smoothOver = 50
	smoothBits = 10

	observeEvery = 10
	blindAfter   = 100
)

var (
	errStreamClosed = errors.New("audio: the stream is closed")
	errStreamDone   = errors.New("audio: the stream has been ended")
)

func frameTime(rate int, n int64) time.Duration {
	r := int64(rate)
	return time.Duration(n/r)*time.Second + time.Duration(n%r)*time.Second/time.Duration(r)
}

func frameCount(rate int, d time.Duration) int64 {
	return int64(d) * int64(rate) / int64(time.Second)
}

type queued struct {
	at    time.Time
	pcm   []int16
	off   int
	eased bool
	soft  bool
	seam  bool
	place int64
}

func blend(block []int16, from, to []int16) {
	frames := len(block) / ChimeChannels
	n := int64(frames) + 1
	for i := range frames {
		for ch := range ChimeChannels {
			step := (int64(to[ch]) - int64(from[ch])) * int64(i+1) / n
			block[i*ChimeChannels+ch] = int16(int64(from[ch]) + step)
		}
	}
}

func (c *queued) frames() int { return len(c.pcm) / ChimeChannels }

func (c *queued) frame(i int) []int16 { return c.pcm[i*ChimeChannels : (i+1)*ChimeChannels] }

func (s *Stream) smoothed(err int64) int64 {
	s.smooth += ((err << smoothBits) - s.smooth) / smoothOver
	return s.smooth >> smoothBits
}

func (s *Stream) ease(err int64) int64 {
	band, step := frameCount(s.rate, deadBand), max(1, frameCount(s.rate, softStep))
	switch {
	case err >= band:
		return min(err, step)
	case err <= -band:
		return max(err, -step)
	default:
		return 0
	}
}

type Stream struct {
	rate   int
	say    func(string, ...any)
	closer func()

	mu     sync.Mutex
	queue  []queued
	held   int64
	index  int64
	blocks int64
	first  time.Time
	done   bool
	ended  int64
	spent  bool
	closed bool

	anchored    bool
	origin      time.Time
	originIndex int64
	ref         int64
	candidates  []time.Time
	bursty      bool
	slipMean    time.Duration
	firstSum    time.Duration
	firstTaken  int
	jump        bool

	asked     int64
	unread    int
	outside   int
	saidBlind bool

	placed  int64
	silence int64
	late    int64
	eased   int64
	slips   int

	began   bool
	lastOut [ChimeChannels]int16
	smooth  int64
	easedAt int64
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
	if s.held+frames > frameCount(s.rate, streamHold) {
		return fmt.Errorf("audio: %s already buffered, so another %d frames passes the %s"+
			" this player holds", frameTime(s.rate, s.held), frames, streamHold)
	}
	if len(s.queue) >= streamChunks {
		return fmt.Errorf("audio: %d chunks are already queued, so another one passes the"+
			" %d this player holds", len(s.queue), streamChunks)
	}
	i := len(s.queue)
	for i > 0 && s.queue[i-1].at.After(at) && s.queue[i-1].off == 0 {
		i--
	}
	if i < len(s.queue) {
		s.queue[i].eased = false
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
	s.done = true
	s.ended += s.discard()
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

func (s *Stream) Placed() (audio, silence time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return frameTime(s.rate, s.placed), frameTime(s.rate, s.silence)
}

func (s *Stream) Spent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent || s.closed
}

func (s *Stream) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discard()
}

func (s *Stream) discard() int64 {
	held := s.held
	s.queue, s.held = nil, 0
	s.began, s.smooth, s.easedAt = false, 0, 0
	return held
}

func (s *Stream) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	placed, silence, late := s.placed, s.silence, s.late
	eased, slips := s.eased, s.slips
	anchored, held, ended := s.anchored, s.held, s.ended
	s.mu.Unlock()

	if anchored {
		s.report("audio: the stream placed %s of audio against %s of silence, dropped %s"+
			" that arrived late and %s that was still buffered when the server ended it,"+
			" eased %s onto the server's clock, and was placed again %d times",
			frameTime(s.rate, placed), frameTime(s.rate, silence), frameTime(s.rate, late),
			frameTime(s.rate, ended), frameTime(s.rate, eased), slips)
	} else {
		s.report("audio: the stream never learned where the player had reached, so it"+
			" played %s of silence and threw away the %s it was holding",
			frameTime(s.rate, silence), frameTime(s.rate, held+ended))
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
	if s.done {
		s.spent = true
		return 0, false
	}
	if s.first.IsZero() {
		s.first = time.Now()
	}
	clear(block)
	frames := len(block) / ChimeChannels
	filled := 0
	blended, trimmed := 0, 0
	if s.anchored {
		filled, blended, trimmed = s.fill(block)
	}
	s.placed += int64(filled)
	s.silence += int64(frames - filled - blended)
	s.eased += int64(blended + trimmed)
	s.index += int64(frames)
	s.blocks++
	return len(block), true
}

func (s *Stream) fill(block []int16) (filled, blended, trimmed int) {
	frames := len(block) / ChimeChannels
	snap := frameCount(s.rate, snapAbove)
	pos := 0
	for pos < frames && len(s.queue) > 0 {
		c := &s.queue[0]
		if len(c.pcm) == 0 {
			s.queue = s.queue[1:]
			continue
		}
		want := s.index + int64(pos)
		if !c.eased {
			err := s.frameAt(c.at) - want
			step := int64(0)
			if held := s.smoothed(err); s.index-s.easedAt >= easeApart {
				step = s.ease(held)
			}
			if step != 0 {
				s.easedAt = s.index
			}
			if !s.began || s.jump || err > snap || err < -snap {
				step, s.smooth = err, 0
				s.jump = s.jump && err <= -int64(c.frames())
			}
			c.eased = true
			c.soft = step != err
			c.place = want + step
		}
		start := c.place + int64(c.off)
		left := int64(c.frames() - c.off)
		switch {
		case start+left <= want:
			s.drop(left)
		case start < want:
			c.seam = true
			if c.soft {
				trimmed += int(want - start)
				s.trim(want - start)
				break
			}
			s.skip(want - start)
		case start > want:
			n := int(min(start-want, int64(frames-pos)))
			if c.soft {
				blend(block[pos*ChimeChannels:(pos+n)*ChimeChannels], s.lastOut[:],
					c.frame(c.off))
				blended += n
			}
			pos += n
		default:
			n := copy(block[pos*ChimeChannels:], c.pcm[c.off*ChimeChannels:]) / ChimeChannels
			if c.soft && c.seam && n > 0 {
				c.seam = false
				for ch := range ChimeChannels {
					at := pos*ChimeChannels + ch
					block[at] = int16((int64(s.lastOut[ch]) + int64(block[at])) / 2)
				}
			}
			copy(s.lastOut[:], block[(pos+n-1)*ChimeChannels:])
			s.began = true
			c.off += n
			s.held -= int64(n)
			pos, filled = pos+n, filled+n
			if c.off == c.frames() {
				s.queue = s.queue[1:]
			}
		}
	}
	if pos < frames {
		s.lastOut = [ChimeChannels]int16{}
	}
	return filled, blended, trimmed
}

func (s *Stream) drop(frames int64) {
	s.late += frames
	s.held -= frames
	s.queue = s.queue[1:]
}

func (s *Stream) trim(frames int64) {
	s.queue[0].off += int(frames)
	s.held -= frames
}

func (s *Stream) skip(frames int64) {
	s.queue[0].off += int(frames)
	s.late += frames
	s.held -= frames
}

func (s *Stream) frameAt(when time.Time) int64 {
	return s.originIndex + frameCount(s.rate, when.Sub(s.origin))
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

	due := p.At.Add(frameTime(s.rate, p.Ahead))
	if p.Bursty != s.bursty {
		s.anchored, s.candidates, s.outside = false, nil, 0
		s.unease()
	}
	s.bursty = p.Bursty
	if !s.anchored {
		if len(s.candidates) == 0 {
			s.ref = s.index
		}
		s.candidates = append(s.candidates, due.Add(-frameTime(s.rate, s.index-s.ref)))
		if len(s.candidates) < anchorTake {
			return 0, 0, false
		}
		slices.SortFunc(s.candidates, time.Time.Compare)
		s.origin, s.originIndex = s.candidates[len(s.candidates)/2], s.ref
		s.candidates, s.anchored, s.slipMean = nil, true, 0
		s.firstSum, s.firstTaken = 0, 0
		return due.Sub(p.At), 0, false
	}
	slip = due.Sub(s.origin.Add(frameTime(s.rate, s.index-s.originIndex)))
	if p.Bursty {
		return s.follow(slip)
	}
	if slip <= anchorSlip && slip >= -anchorSlip {
		s.outside = 0
		return 0, 0, false
	}
	if s.outside++; s.outside < slipRuns {
		return 0, 0, false
	}
	s.origin, s.originIndex = due, s.index
	s.unease()
	s.outside, s.slips = 0, s.slips+1
	return 0, slip, true
}

func (s *Stream) follow(slip time.Duration) (depth, moved time.Duration, again bool) {
	if slip > burstySlip || slip < -burstySlip {
		if s.outside++; s.outside < slipRuns {
			return 0, 0, false
		}
		s.origin = s.origin.Add(slip)
		s.firstSum, s.firstTaken, s.slipMean, s.outside = 0, 0, 0, 0
		s.jump, s.slips = true, s.slips+1
		s.unease()
		return 0, slip, true
	}
	s.outside = 0
	if s.firstTaken < burstyFirst {
		s.firstSum += slip
		if s.firstTaken++; s.firstTaken < burstyFirst {
			return 0, 0, false
		}
		moved = s.firstSum / burstyFirst
		s.origin, s.jump = s.origin.Add(moved), true
		s.unease()
		return 0, 0, false
	}
	s.slipMean += (slip - s.slipMean) / burstyOver
	if s.slipMean <= burstyBand && s.slipMean >= -burstyBand {
		return 0, 0, false
	}
	moved, s.slipMean = s.slipMean, 0
	s.origin = s.origin.Add(moved)
	s.unease()
	return 0, 0, false
}

func (s *Stream) unease() {
	for i := range s.queue {
		if s.queue[i].off == 0 {
			s.queue[i].eased = false
		}
	}
}
