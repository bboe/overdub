package sendspin

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	binaryPlayerFirst byte = 4
	binaryPlayerLast  byte = 7
	binaryAudioChunk  byte = 4

	chunkStampBytes     = 8
	chunkSendAheadBytes = 4
	chunkHeadBytes      = chunkStampBytes + chunkSendAheadBytes
	frameBytes          = StreamChannels * StreamBitDepth / 8

	sendAheadUnmeasured = math.MaxUint32
	maxDelaySamples     = 4096
)

type audioChunk struct {
	ServerTime int64
	SendAhead  int64
	Arrived    int64
	PCM        []byte
}

func (c *audioChunk) Frames() int { return len(c.PCM) / frameBytes }

func playerBinary(kind byte) bool {
	return kind >= binaryPlayerFirst && kind <= binaryPlayerLast
}

func chunkStamp(body []byte) (int64, error) {
	if len(body) < chunkHeadBytes {
		return 0, fmt.Errorf("%w: an audio chunk too short to carry its timestamp and"+
			" send-ahead", errTransport)
	}
	stamp := int64(binary.BigEndian.Uint64(body[:chunkStampBytes]))
	if !onAClock(stamp) {
		return 0, fmt.Errorf("%w: an audio chunk due off any clock this player keeps",
			errTransport)
	}
	return stamp, nil
}

func chunkSendAhead(body []byte) int64 {
	return int64(binary.BigEndian.Uint32(body[chunkStampBytes:chunkHeadBytes]))
}

func parseChunk(body []byte) (*audioChunk, error) {
	stamp, err := chunkStamp(body)
	if err != nil {
		return nil, err
	}
	pcm := body[chunkHeadBytes:]
	if len(pcm)%frameBytes != 0 {
		return nil, fmt.Errorf("%w: audio that is not a whole number of %d-byte frames",
			errTransport, frameBytes)
	}
	return &audioChunk{ServerTime: stamp, SendAhead: chunkSendAhead(body), PCM: pcm}, nil
}

func parseFLACChunk(body []byte, rate int) (*audioChunk, error) {
	stamp, err := chunkStamp(body)
	if err != nil {
		return nil, err
	}
	pcm, err := decodeFLAC(body[chunkHeadBytes:], rate)
	if err != nil {
		return nil, err
	}
	return &audioChunk{ServerTime: stamp, SendAhead: chunkSendAhead(body), PCM: pcm}, nil
}

func (s *Session) AudioChunk(body []byte) (*audioChunk, error) {
	if !s.streaming {
		return nil, nil
	}
	arrived := nowMicros()
	parse := parseChunk
	if s.flac {
		parse = func(body []byte) (*audioChunk, error) { return parseFLACChunk(body, s.rate) }
	}
	c, err := parse(body)
	if err != nil {
		return nil, err
	}
	c.Arrived = arrived
	return c, nil
}

func (s *Session) ArrivalDelay(c *audioChunk) (int64, bool) {
	if s.clock == nil || c.SendAhead == 0 || c.SendAhead == sendAheadUnmeasured {
		return 0, false
	}
	sent, _, _, ok := s.clock.filter.sample(c.ServerTime - c.SendAhead)
	if !ok {
		return 0, false
	}
	return c.Arrived - sent, true
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

	delays []int64

	every time.Duration
	due   time.Time

	clockKnown bool
	leadKnown  bool
	spread     int64
	firstOff   int64
	lastOff    int64

	play        *playback
	lastStream  Stream
	lastPlaced  time.Duration
	lastSilence time.Duration
}

func (r *chunkRun) placedSince() (audio, silence time.Duration) {
	if r.play == nil {
		return 0, 0
	}
	if r.play.stream != r.lastStream {
		r.lastStream, r.lastPlaced, r.lastSilence = r.play.stream, 0, 0
	}
	placed, quiet := r.play.counts()
	audio, silence = placed-r.lastPlaced, quiet-r.lastSilence
	r.lastPlaced, r.lastSilence = placed, quiet
	return audio, silence
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
	if d, ok := s.ArrivalDelay(c); ok && len(r.delays) < maxDelaySamples {
		r.delays = append(r.delays, d)
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

func (r *chunkRun) delaySet() string {
	if r.play == nil {
		return ""
	}
	held := r.play.delay()
	if held == 0 {
		return ""
	}
	return ", placed " + held.String() + " earlier than stamped for this player's output" +
		" delay"
}

func (r *chunkRun) arrivals() string {
	if len(r.delays) == 0 {
		return ""
	}
	d := slices.Clone(r.delays)
	slices.Sort(d)
	at := func(q float64) time.Duration { return micros(d[int(q*float64(len(d)-1))]) }
	return fmt.Sprintf("; %d of them arrived %s after they were sent at the median, %s"+
		" at the 95th percentile, %s at the 99th and %s at most", len(d), at(0.5),
		at(0.95), at(0.99), at(1))
}

func (r *chunkRun) report(peer *untrustedlog.Log, name string) {
	if r.chunks == 0 {
		return
	}
	audio, silence := r.placedSince()
	if r.leadKnown {
		peer.Printf("sendspin: %q sent %d chunks, %d frames, %d bytes, due between"+
			" %s and %s ahead, against a clock good to %s that moved %s; the player"+
			" placed %s of audio against %s of silence%s", name,
			r.chunks, r.frames, r.bytes, micros(r.leastLead), micros(r.mostLead),
			micros(r.spread), micros(r.lastOff-r.firstOff), audio, silence, r.delaySet()+
				r.arrivals())
	} else {
		peer.Printf("sendspin: %q sent %d chunks, %d frames, %d bytes, with no clock"+
			" to say when they were due; the player placed %s of audio against %s of"+
			" silence%s", name, r.chunks, r.frames, r.bytes, audio, silence, r.delaySet())
	}
	*r = chunkRun{announced: r.announced, every: r.every, due: time.Now().Add(r.every),
		clockKnown: r.clockKnown, firstOff: r.lastOff, lastOff: r.lastOff,
		play: r.play, lastStream: r.lastStream,
		lastPlaced: r.lastPlaced, lastSilence: r.lastSilence}
}

func (r *chunkRun) done(peer *untrustedlog.Log, name string) {
	r.report(peer, name)
	*r = chunkRun{every: r.every, play: r.play, lastStream: r.lastStream,
		lastPlaced: r.lastPlaced, lastSilence: r.lastSilence}
}
