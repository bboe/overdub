package sendspin

import "time"

type Player interface {
	OpenStream(rate int, say func(string, ...any)) (Stream, error)
}

type Stream interface {
	Write(at time.Time, pcm []byte) error
	Clear()
	Finish()
	Resume() bool
	Spent() bool
	Placed() (audio, silence time.Duration)
	Close()
}

func (s *Session) When(serverTime int64) (time.Time, bool) {
	if s.clock == nil {
		return time.Time{}, false
	}
	client, _, _, ok := s.clock.filter.sample(serverTime)
	if !ok {
		return time.Time{}, false
	}
	return monotonicStart.Add(time.Duration(client) * time.Microsecond), true
}

type playback struct {
	player Player
	say    func(string, ...any)
	name   string

	stream Stream
	rate   int
	held   func() time.Duration

	saidShut    bool
	saidNoClock bool
	saidRefused bool
}

func (p *playback) open(rate int) {
	if p.stream != nil && p.rate == rate && p.stream.Resume() {
		return
	}
	p.stop()
	if p.player == nil {
		if !p.saidShut {
			p.saidShut = true
			p.say("sendspin: this dot has no player, so the stream %q opened is dropped"+
				" rather than played", p.name)
		}
		return
	}
	stream, err := p.player.OpenStream(rate, p.say)
	if err != nil {
		if !p.saidShut {
			p.saidShut = true
			p.say("sendspin: the player would not open a stream for %q, so its audio is"+
				" dropped: %v", p.name, err)
		}
		return
	}
	p.stream, p.rate = stream, rate
}

func (p *playback) take(s *Session, c *audioChunk) {
	if p.stream == nil {
		return
	}
	at, ok := s.When(c.ServerTime)
	if !ok {
		if !p.saidNoClock {
			p.saidNoClock = true
			p.say("sendspin: %q sent audio before this player's clock agreed with its"+
				" own, so there is no moment of ours to play it at", p.name)
		}
		return
	}
	if err := p.stream.Write(at.Add(-p.delay()), c.PCM); err != nil {
		if !p.saidRefused {
			p.saidRefused = true
			p.say("sendspin: %q sent audio this player will not hold: %v", p.name, err)
		}
	}
}

func (p *playback) delay() time.Duration {
	if p.held == nil {
		return 0
	}
	return p.held()
}

func (p *playback) counts() (audio, silence time.Duration) {
	if p.stream == nil {
		return 0, 0
	}
	return p.stream.Placed()
}

func (p *playback) clear() {
	if p.stream != nil {
		p.stream.Clear()
	}
}

func (p *playback) finish() {
	if p.stream != nil {
		p.stream.Finish()
	}
}

func (p *playback) stop() {
	if p.stream == nil {
		return
	}
	p.stream.Close()
	p.stream = nil
}
