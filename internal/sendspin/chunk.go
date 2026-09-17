package sendspin

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	binaryPlayerFirst byte = 4
	binaryPlayerLast  byte = 7
	binaryAudioChunk  byte = 4

	chunkStampBytes = 8
	frameBytes      = StreamChannels * StreamBitDepth / 8
)

type audioChunk struct {
	ServerTime int64
	PCM        []byte
}

func (c *audioChunk) Frames() int { return len(c.PCM) / frameBytes }

func playerBinary(kind byte) bool {
	return kind >= binaryPlayerFirst && kind <= binaryPlayerLast
}

func parseChunk(body []byte) (*audioChunk, error) {
	if len(body) < chunkStampBytes {
		return nil, fmt.Errorf("%w: an audio chunk too short to carry its timestamp",
			errTransport)
	}
	stamp := int64(binary.BigEndian.Uint64(body[:chunkStampBytes]))
	if !onAClock(stamp) {
		return nil, fmt.Errorf("%w: an audio chunk due off any clock this player keeps",
			errTransport)
	}
	pcm := body[chunkStampBytes:]
	if len(pcm)%frameBytes != 0 {
		return nil, fmt.Errorf("%w: audio that is not a whole number of %d-byte frames",
			errTransport, frameBytes)
	}
	return &audioChunk{ServerTime: stamp, PCM: pcm}, nil
}

func (s *Session) AudioChunk(body []byte) (*audioChunk, error) {
	if !s.streaming {
		return nil, nil
	}
	return parseChunk(body)
}

func (s *Session) Lead(serverTime int64) (lead, spread, offset int64, ok bool) {
	if s.clock == nil {
		return 0, 0, 0, false
	}
	client, spread, offset, ok := s.clock.filter.sample(serverTime)
	if !ok {
		return 0, 0, 0, false
	}
	return client - nowMicros(), spread, offset, true
}

const reportEvery = 30 * time.Second

func micros(us int64) time.Duration { return time.Duration(us) * time.Microsecond }

type chunkRun struct {
	announced bool

	chunks int
	frames int
	bytes  int

	leastLead int64
	mostLead  int64

	every time.Duration
	due   time.Time

	clockKnown bool
	leadKnown  bool
	spread     int64
	firstOff   int64
	lastOff    int64
}

func (r *chunkRun) took(peer *untrustedlog.Log, name string, s *Session, c *audioChunk) {
	r.chunks++
	r.frames += c.Frames()
	r.bytes += len(c.PCM)
	lead, spread, offset, known := s.Lead(c.ServerTime)
	if known {
		if !r.clockKnown {
			r.clockKnown, r.firstOff = true, offset
		}
		r.spread, r.lastOff = spread, offset
		if !r.leadKnown || lead < r.leastLead {
			r.leastLead = lead
		}
		if !r.leadKnown || lead > r.mostLead {
			r.mostLead = lead
		}
		r.leadKnown = true
	}
	if !r.announced {
		r.announced = true
		if known {
			peer.Printf("sendspin: %q sent its first chunk, %d frames due in %s",
				name, c.Frames(), micros(lead))
		} else {
			peer.Printf("sendspin: %q sent its first chunk, %d frames, with no clock yet"+
				" to say when it is due", name, c.Frames())
		}
	}
	if r.due.IsZero() {
		r.due = time.Now().Add(r.every)
	}
}

func (r *chunkRun) tick(peer *untrustedlog.Log, name string) {
	if r.chunks == 0 || r.due.IsZero() || time.Now().Before(r.due) {
		return
	}
	r.report(peer, name)
}

func (r *chunkRun) report(peer *untrustedlog.Log, name string) {
	if r.chunks == 0 {
		return
	}
	if r.leadKnown {
		peer.Printf("sendspin: %q sent %d chunks, %d frames, %d bytes, due between"+
			" %s and %s ahead, against a clock good to %s that moved %s", name,
			r.chunks, r.frames, r.bytes, micros(r.leastLead), micros(r.mostLead),
			micros(r.spread), micros(r.lastOff-r.firstOff))
	} else {
		peer.Printf("sendspin: %q sent %d chunks, %d frames, %d bytes, with no clock"+
			" to say when they were due", name, r.chunks, r.frames, r.bytes)
	}
	*r = chunkRun{announced: r.announced, every: r.every, due: time.Now().Add(r.every),
		clockKnown: r.clockKnown, firstOff: r.lastOff, lastOff: r.lastOff}
}

func (r *chunkRun) done(peer *untrustedlog.Log, name string) {
	r.report(peer, name)
	*r = chunkRun{every: r.every}
}
