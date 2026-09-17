package sendspin

import "time"

type Player interface {
	OpenStream(say func(string, ...any)) (Stream, error)
}

type Stream interface {
	Write(at time.Time, pcm []byte) error
	Clear()
	Finish()
	Resume() bool
	Spent() bool
	Placed() (audio, silence int64)
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
	delay  time.Duration

	saidShut    bool
	saidNoClock bool
	saidRefused bool
}

func (p *playback) open() {
	if p.stream != nil && p.stream.Resume() {
		return
	}
	p.stream = nil
	if p.player == nil {
		if !p.saidShut {
			p.saidShut = true
			p.say("sendspin: this dot has no player, so the stream %q opened is dropped"+
				" rather than played", p.name)
		}
		return
	}
	stream, err := p.player.OpenStream(p.say)
	if err != nil {
		if !p.saidShut {
			p.saidShut = true
			p.say("sendspin: the player would not open a stream for %q, so its audio is"+
				" dropped: %v", p.name, err)
		}
		return
	}
	p.stream = stream
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
	if err := p.stream.Write(at.Add(-p.delay), c.PCM); err != nil {
		if !p.saidRefused {
			p.saidRefused = true
			p.say("sendspin: %q sent audio this player will not hold: %v", p.name, err)
		}
	}
}

func (p *playback) counts() (audio, silence int64) {
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
