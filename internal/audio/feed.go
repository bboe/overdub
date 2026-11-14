package audio

import (
	"errors"
	"sync"
	"time"
)

const (
	feedQueue    = 2 * time.Second
	feedPrebuild = 300 * time.Millisecond
)

var ErrFeedStopped = errors.New("audio: the clip was stopped")

type Feed struct {
	from  int
	to    func() int
	cutTo func() float64
	done  func()

	mu      sync.Mutex
	room    *sync.Cond
	pcm     []int16
	at      int
	pos     float64
	started bool
	ended   bool
	stopped bool
	told    bool
	cut     float64
}

func newFeed(from int, to func() int, done func()) *Feed {
	f := &Feed{from: from, to: to, done: done}
	f.room = sync.NewCond(&f.mu)
	return f
}

func (f *Feed) queued() int { return (len(f.pcm) - f.at) / ChimeChannels }

func (f *Feed) Write(pcm []int16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	limit := int(frameCount(f.from, feedQueue))
	for !f.stopped && f.queued() >= limit {
		f.room.Wait()
	}
	if f.stopped {
		return ErrFeedStopped
	}
	if f.at > 0 && f.at >= len(f.pcm)/2 {
		f.pcm = append(f.pcm[:0], f.pcm[f.at:]...)
		f.at = 0
	}
	f.pcm = append(f.pcm, pcm[:len(pcm)/ChimeChannels*ChimeChannels]...)
	return nil
}

func (f *Feed) Finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = true
}

func (f *Feed) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	f.room.Broadcast()
}

func (f *Feed) read(block []int16) (int, bool) {
	f.mu.Lock()
	n, more := f.fill(block)
	if f.cutTo != nil {
		scaleToward(f.to(), &f.cut, min(max(f.cutTo(), 0), 1), block[:n])
	}
	tell := !more && !f.told
	f.told = f.told || tell
	f.mu.Unlock()
	if tell && f.done != nil {
		go f.done()
	}
	return n, more
}

func (f *Feed) fill(block []int16) (int, bool) {
	if f.stopped {
		return 0, false
	}
	if !f.started {
		if !f.ended && f.queued() < int(frameCount(f.from, feedPrebuild)) {
			return 0, true
		}
		f.started = true
	}
	to := f.to()
	step := float64(f.from) / float64(to)
	frames := len(block) / ChimeChannels
	n := 0
	for n < frames {
		i := int(f.pos)
		frac := f.pos - float64(i)
		last := i
		if frac > 0 {
			last = i + 1
		}
		if last >= f.queued() {
			break
		}
		for ch := range ChimeChannels {
			a := float64(f.pcm[f.at+i*ChimeChannels+ch])
			b := a
			if frac > 0 {
				b = float64(f.pcm[f.at+(i+1)*ChimeChannels+ch])
			}
			block[n*ChimeChannels+ch] = int16(a + (b-a)*frac)
		}
		f.pos += step
		n++
	}
	whole := int(f.pos)
	f.at += whole * ChimeChannels
	f.pos -= float64(whole)
	f.room.Broadcast()
	if f.ended && f.queued() <= 1 && n < frames {
		return n * ChimeChannels, false
	}
	return n * ChimeChannels, true
}
