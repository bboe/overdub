package esphome

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	msgSubscribeVoice = 89
	msgVoiceRequest   = 90
	msgVoiceResponse  = 91
	msgVoiceEvent     = 92
	msgVoiceAudio     = 106
	msgVoiceFinished  = 120
)

const (
	voiceFeatureAssistant = 1 << 0
	voiceFeatureSpeaker   = 1 << 1
	voiceFeatureAPIAudio  = 1 << 2
	voiceFeatureAnnounce  = 1 << 4

	voiceFeatures = voiceFeatureAssistant | voiceFeatureAPIAudio
)

const (
	voiceEventError     = 0
	voiceEventRunStart  = 1
	voiceEventRunEnd    = 2
	voiceEventSTTEnd    = 4
	voiceEventIntentEnd = 6
	voiceEventTTSStart  = 7
	voiceEventTTSEnd    = 8
	voiceEventVADEnd    = 12
)

var errNoSatellite = errors.New("esphome: home assistant has not subscribed to voice")

type Answer struct {
	Transcript string
	Speech     string
	URL        string
	Err        error
}

type Turn struct {
	srv  *Server
	conn *conn

	heard    sync.Once
	ended    chan struct{}
	finished sync.Once
	done     chan struct{}

	mu      sync.Mutex
	running bool
	answer  Answer
}

func (s *Server) UseSatellite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.satellite = true
}

func (s *Server) SatelliteReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voiceConn != nil
}

func (s *Server) StartTurn(ctx context.Context) (*Turn, error) {
	s.mu.Lock()
	if s.voiceConn == nil {
		s.mu.Unlock()
		return nil, errNoSatellite
	}
	if s.turn != nil {
		s.mu.Unlock()
		return nil, errors.New("esphome: a turn is already running")
	}
	t := &Turn{srv: s, conn: s.voiceConn, ended: make(chan struct{}), done: make(chan struct{})}
	var req pb
	req.boolean(1, true) // start
	if err := s.send(t.conn, msgVoiceRequest, req.b); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.turn = t
	s.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			t.abort(ctx.Err())
		case <-t.done:
		}
	}()
	return t, nil
}

func (t *Turn) Send(pcm []int16) error {
	buf := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	var msg pb
	msg.sub(1, buf) // data
	return t.sendIfCurrent(msgVoiceAudio, msg.b)
}

func (t *Turn) End() error {
	var msg pb
	msg.boolean(2, true) // end
	return t.sendIfCurrent(msgVoiceAudio, msg.b)
}

func (t *Turn) SpeechEnded() <-chan struct{} { return t.ended }

func (t *Turn) Await() Answer {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.answer
}

func (t *Turn) Close() error {
	select {
	case <-t.done:
	default:
		t.abort(errors.New("esphome: the turn was closed before it finished"))
	}
	s := t.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == t {
		s.turn = nil
	}
	if _, open := s.conns[t.conn]; !open {
		return errNoSatellite
	}
	var msg pb
	msg.boolean(1, true) // success
	return s.send(t.conn, msgVoiceFinished, msg.b)
}

func (t *Turn) sendIfCurrent(msgType int, payload []byte) error {
	s := t.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != t {
		return errNoSatellite
	}
	return s.send(t.conn, msgType, payload)
}

func (t *Turn) abort(err error) {
	var req pb
	req.boolean(1, false) // start
	_ = t.sendIfCurrent(msgVoiceRequest, req.b)
	t.fail(err)
}

func (t *Turn) fail(err error) {
	t.mu.Lock()
	if t.answer.Err == nil {
		t.answer.Err = err
	}
	t.mu.Unlock()
	t.finish()
}

func (t *Turn) hear() { t.heard.Do(func() { close(t.ended) }) }

func (t *Turn) finish() {
	t.hear()
	t.finished.Do(func() { close(t.done) })
}

func (t *Turn) event(kind int, data map[string]string) {
	t.mu.Lock()
	if kind == voiceEventRunStart {
		t.running = true
	}
	if !t.running {
		t.mu.Unlock()
		return
	}
	switch kind {
	case voiceEventSTTEnd:
		t.answer.Transcript = data["text"]
	case voiceEventIntentEnd:
		t.answer.Speech = data["speech"]
	case voiceEventTTSStart:
		if text := data["text"]; text != "" {
			t.answer.Speech = text
		}
	case voiceEventTTSEnd:
		t.answer.URL = data["url"]
	case voiceEventError:
		if t.answer.Err == nil {
			t.answer.Err = fmt.Errorf("esphome: home assistant's pipeline failed: %s: %s",
				untrustedlog.Cut(data["code"]), untrustedlog.Cut(data["message"]))
		}
	}
	t.mu.Unlock()
	switch kind {
	case voiceEventSTTEnd, voiceEventVADEnd:
		t.hear()
	case voiceEventError, voiceEventRunEnd:
		t.finish()
	}
}

func (s *Server) dropVoiceLocked(conn *conn) {
	if s.voiceConn == conn {
		s.voiceConn = nil
	}
	if s.turn != nil && s.turn.conn == conn {
		s.turn.fail(errors.New("esphome: home assistant left during the turn"))
		s.turn = nil
	}
}

func (s *Server) handleVoice(conn *conn, msgType int, payload []byte) error {
	switch msgType {
	case msgSubscribeVoice:
		var subscribe bool
		if err := walk("SubscribeVoiceAssistantRequest", payload, func(f pbField) {
			if f.field == 1 {
				subscribe = f.num != 0
			}
		}); err != nil {
			return err
		}
		if subscribe {
			if s.voiceConn != nil && s.voiceConn != conn {
				s.dropVoiceLocked(s.voiceConn)
			}
			s.voiceConn = conn
			conn.noted = fmt.Sprintf("esphome api: %s subscribed to voice", conn.sock.RemoteAddr())
		} else if s.voiceConn == conn {
			s.dropVoiceLocked(conn)
			conn.noted = fmt.Sprintf("esphome api: %s unsubscribed from voice", conn.sock.RemoteAddr())
		}
		return nil

	case msgVoiceResponse:
		var failed bool
		if err := walk("VoiceAssistantResponse", payload, func(f pbField) {
			if f.field == 2 {
				failed = f.num != 0
			}
		}); err != nil {
			return err
		}
		if failed && s.turn != nil && s.turn.conn == conn {
			s.turn.fail(errors.New("esphome: home assistant refused the pipeline"))
		}
		return nil

	case msgVoiceEvent:
		kind := 0
		data := map[string]string{}
		var inner error
		if err := walk("VoiceAssistantEventResponse", payload, func(f pbField) {
			switch f.field {
			case 1:
				kind = int(f.num)
			case 2:
				var name, value string
				if err := walk("VoiceAssistantEventData", f.data, func(g pbField) {
					switch g.field {
					case 1:
						name = string(g.data)
					case 2:
						value = string(g.data)
					}
				}); err != nil {
					inner = err
				}
				data[name] = value
			}
		}); err != nil {
			return err
		}
		if inner != nil {
			return inner
		}
		if s.turn != nil && s.turn.conn == conn {
			s.turn.event(kind, data)
		}
		return nil
	}
	return nil
}
