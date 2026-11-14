// Package clip plays a media player URL on the mixer: Home Assistant transcodes
// it to FLAC, this package fetches and decodes it, and internal/audio mixes it.
// docs/api.md has the route and why it is not Alexa's.
package clip

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bboe/overdub/internal/audio"
	"github.com/bboe/overdub/internal/flacread"
)

const (
	Rate     = audio.ChimeRate
	Channels = flacread.Channels
	Depth    = flacread.Depth

	maxURL     = 2048
	readBuffer = 64 << 10
	maxBlock   = 16384
)

var (
	errNotFLAC   = errors.New("clip: the reply is not FLAC")
	errFormat    = fmt.Errorf("clip: the FLAC is not %d Hz, %d channels, %d bit", Rate, Channels, Depth)
	errFrame     = errors.New("clip: a FLAC frame that does not decode")
	errFrameSize = fmt.Errorf("clip: a FLAC frame of more than %d samples", maxBlock)
)

func flacError(err error) error {
	switch {
	case errors.Is(err, flacread.ErrHeader):
		return errNotFLAC
	case errors.Is(err, flacread.ErrFormat):
		return errFormat
	case errors.Is(err, flacread.ErrLong):
		return errFrameSize
	case errors.Is(err, flacread.ErrFrame):
		return errFrame
	}
	return err
}

var stallAfter = 15 * time.Second

type Feed interface {
	Write(pcm []int16) error
	Finish()
	Stop()
}

type Player struct {
	Open    func(rate int, done func()) (Feed, error)
	Client  *http.Client
	OnStart func(announcement bool)
	OnEnd   func(announcement bool, err error)

	tell    sync.Mutex
	mu      sync.Mutex
	current *play
}

type play struct {
	announcement bool
	cancel       context.CancelFunc

	mu      sync.Mutex
	feed    Feed
	stopped bool
	stalled atomic.Bool
}

func (p *play) setFeed(f Feed) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.feed = f
	return !p.stopped
}

func (p *play) stop() {
	p.cancel()
	p.mu.Lock()
	p.stopped = true
	f := p.feed
	p.mu.Unlock()
	if f != nil {
		f.Stop()
	}
}

func CheckURL(target string) error {
	if len(target) > maxURL {
		return fmt.Errorf("clip: a url of %d bytes, over %d", len(target), maxURL)
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return errors.New("clip: the url is not http or https")
	}
	return nil
}

func (p *Player) Play(target string, announcement bool) error {
	if err := CheckURL(target); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	next := &play{announcement: announcement, cancel: cancel}
	p.swap(next, func(*play) bool { return true })
	go p.run(ctx, next, target)
	return nil
}

func (p *Player) Stop() { p.swap(nil, func(*play) bool { return true }) }

func (p *Player) Yield() { p.swap(nil, func(pl *play) bool { return !pl.announcement }) }

func (p *Player) swap(next *play, ends func(*play) bool) {
	p.tell.Lock()
	defer p.tell.Unlock()
	p.mu.Lock()
	old := p.current
	if old != nil && !ends(old) {
		p.mu.Unlock()
		return
	}
	p.current = next
	p.mu.Unlock()
	if old == nil {
		return
	}
	old.stop()
	if p.OnEnd != nil {
		p.OnEnd(old.announcement, nil)
	}
}

func (p *Player) end(pl *play, err error) {
	p.tell.Lock()
	defer p.tell.Unlock()
	p.mu.Lock()
	current := p.current == pl
	if current {
		p.current = nil
	}
	p.mu.Unlock()
	if current && p.OnEnd != nil {
		p.OnEnd(pl.announcement, err)
	}
}

func (p *Player) started(pl *play) {
	p.tell.Lock()
	defer p.tell.Unlock()
	p.mu.Lock()
	current := p.current == pl
	p.mu.Unlock()
	if current && p.OnStart != nil {
		p.OnStart(pl.announcement)
	}
}

func (p *Player) run(ctx context.Context, pl *play, target string) {
	watch := time.AfterFunc(stallAfter, func() {
		pl.stalled.Store(true)
		pl.stop()
	})
	playing := false
	defer func() {
		if !playing {
			watch.Stop()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		p.end(pl, err)
		return
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		p.end(pl, p.why(ctx, pl, fmt.Errorf("clip: fetch: %w", err)))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.end(pl, fmt.Errorf("clip: HTTP %d", resp.StatusCode))
		return
	}
	r := bufio.NewReaderSize(resp.Body, readBuffer)
	if err := flacread.ReadHeader(r, Rate); err != nil {
		p.end(pl, p.why(ctx, pl, flacError(err)))
		return
	}
	feed, err := p.Open(Rate, func() {
		watch.Stop()
		p.end(pl, p.why(ctx, pl, nil))
	})
	if err != nil {
		p.end(pl, err)
		return
	}
	if !pl.setFeed(feed) {
		feed.Stop()
		return
	}
	var pcm []int16
	for first := true; ; first = false {
		pcm, err = flacread.Next(r, Rate, maxBlock, pcm[:0])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			p.end(pl, p.why(ctx, pl, flacError(err)))
			feed.Stop()
			return
		}
		if first {
			p.started(pl)
		}
		if feed.Write(pcm) != nil {
			if pl.stalled.Load() {
				p.end(pl, p.why(ctx, pl, nil))
			}
			return
		}
		watch.Reset(stallAfter)
	}
	playing = true
	watch.Reset(stallAfter)
	feed.Finish()
}

func (p *Player) why(ctx context.Context, pl *play, err error) error {
	if pl.stalled.Load() {
		return fmt.Errorf("clip: nothing moved for %v, so the clip was ended", stallAfter)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}
