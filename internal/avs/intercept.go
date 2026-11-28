package avs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/bboe/overdub/internal/esphome"
	"github.com/bboe/overdub/internal/opus"
	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	eventsPath    = "/v20160207/events"
	replyBoundary = "------abcde123"

	maxListen   = 10 * time.Second
	answerLimit = 2 * time.Minute

	sendSamples = opus.Rate / 10

	noSpeech = "stt-no-text-recognized"
)

type Turn interface {
	Send(pcm []int16) error
	End() error
	SpeechEnded() <-chan struct{}
	Done() <-chan struct{}
	Await() esphome.Answer
	Close() error
}

type Answerer struct {
	Start    func(ctx context.Context) (Turn, error)
	Announce func(url string) (<-chan struct{}, error)
	Fail     func()
}

type decoder interface {
	Decode(packet []byte) ([]int16, error)
	Close()
}

func openOpus() (decoder, error) {
	d, err := opus.NewDecoder()
	if err != nil {
		return nil, err
	}
	return d, nil
}

var turnLimit = 30 * time.Second

func (r *Relay) UseAnswerer(a Answerer) { r.answer = a }

func (r *Relay) Intercept(on bool) { r.intercepting.Store(on) }

type event struct {
	namespace   string
	name        string
	dialogID    string
	startedAt   string
	wakeWordEnd int
}

func parseEvent(part []byte) (event, bool) {
	var msg struct {
		Event struct {
			Header struct {
				Namespace       string `json:"namespace"`
				Name            string `json:"name"`
				DialogRequestID string `json:"dialogRequestId"`
			} `json:"header"`
			Payload struct {
				Initiator struct {
					Payload struct {
						WakeWordIndices struct {
							End int `json:"endIndexInSamples"`
						} `json:"wakeWordIndices"`
					} `json:"payload"`
				} `json:"initiator"`
				StartOfSpeechTimestamp string `json:"startOfSpeechTimestamp"`
			} `json:"payload"`
		} `json:"event"`
	}
	if json.Unmarshal(part, &msg) != nil || msg.Event.Header.Name == "" {
		return event{}, false
	}
	h, p := msg.Event.Header, msg.Event.Payload
	return event{
		namespace:   h.Namespace,
		name:        h.Name,
		dialogID:    h.DialogRequestID,
		startedAt:   p.StartOfSpeechTimestamp,
		wakeWordEnd: p.Initiator.Payload.WakeWordIndices.End,
	}, true
}

type upload struct {
	chunks <-chan []byte
	err    error
	held   []byte
	keep   bool
	audio  []byte
	w      *watcher
	ev     event
	parsed bool
}

func newUpload(ctx context.Context, body io.Reader, boundary string) *upload {
	chunks := make(chan []byte)
	u := &upload{chunks: chunks, keep: true}
	u.w = newWatcher(boundary, func(part []byte) {
		if !u.parsed {
			u.ev, u.parsed = parseEvent(part)
		}
	})
	u.w.onAudio = func(b []byte) { u.audio = append(u.audio, b...) }
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 16<<10)
			n, err := body.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					u.err = err
				}
				return
			}
		}
	}()
	return u
}

func (u *upload) take(chunk []byte) {
	if u.keep {
		u.held = append(u.held, chunk...)
	}
	u.w.Write(chunk)
}

func (u *upload) peek() {
	for !u.parsed && len(u.held) <= maxHead+maxJSON {
		chunk, ok := <-u.chunks
		if !ok {
			return
		}
		u.take(chunk)
	}
}

func (u *upload) drain() {
	for range u.chunks {
	}
}

func (u *upload) Read(p []byte) (int, error) {
	for len(u.held) == 0 {
		chunk, ok := <-u.chunks
		if !ok {
			if u.err != nil {
				return 0, u.err
			}
			return 0, io.EOF
		}
		u.held = chunk
	}
	n := copy(p, u.held)
	u.held = u.held[n:]
	return n, nil
}

func (r *Relay) intercept(w http.ResponseWriter, req *http.Request, id int64) (io.Reader, bool) {
	boundary := boundaryOf(req.Header.Get("Content-Type"))
	if boundary == "" {
		return req.Body, false
	}
	u := newUpload(req.Context(), req.Body, boundary)
	u.peek()
	switch {
	case !u.parsed:
		return u, false
	case u.ev.namespace == "Alexa.AudioFrontEndController":
		u.drain()
		w.WriteHeader(http.StatusNoContent)
		log.Printf("avs: req %d: kept %s from Amazon", id, untrustedlog.Cut(u.ev.name))
		return nil, true
	case u.ev.namespace == "SpeechRecognizer" && u.ev.name == "Recognize":
		u.keep, u.held = false, nil
		r.answerTurn(w, req, id, u)
		return nil, true
	}
	return u, false
}

func (r *Relay) answerTurn(w http.ResponseWriter, req *http.Request, id int64, u *upload) {
	if r.answer.Start == nil {
		r.decline(w, id, u, "home assistant has no satellite here", true)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), turnLimit)
	defer cancel()
	t, err := r.answer.Start(ctx)
	if err != nil {
		r.decline(w, id, u, err.Error(), !errors.Is(err, esphome.ErrTurnRunning))
		return
	}
	dec, err := r.decoder()
	if err != nil {
		t.Close()
		r.decline(w, id, u, err.Error(), true)
		return
	}
	defer dec.Close()
	log.Printf("avs: req %d: home assistant takes the turn", id)

	skip := opus.NewSkip(u.ev.wakeWordEnd)
	var pending []int16
	heard := 0
	feed := func() {
		packets, rest := opus.Split(u.audio)
		for _, packet := range packets {
			pcm, err := dec.Decode(packet)
			if err != nil {
				continue
			}
			heard += len(pcm)
			pending = append(pending, skip.Apply(pcm)...)
		}
		u.audio = append(u.audio[:0], rest...)
		if len(pending) >= sendSamples {
			t.Send(pending)
			pending = pending[:0]
		}
	}
	feed()

	limit := time.NewTimer(maxListen)
	defer limit.Stop()
	chunks := u.chunks
listening:
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				break listening
			}
			u.take(chunk)
			feed()
		case <-t.SpeechEnded():
			break listening
		case <-limit.C:
			log.Printf("avs: req %d: no end of speech from home assistant in %v", id, maxListen)
			break listening
		}
	}

	if err := req.Context().Err(); err != nil {
		t.Close()
		log.Printf("avs: req %d: alexa gave up on the turn: %v", id, err)
		return
	}
	if len(pending) > 0 {
		t.Send(pending)
	}
	t.End()
	go u.drain()

	out := openReply(w, u.ev.dialogID)
	out.directive("SpeechRecognizer", "SetEndOfSpeechOffset", map[string]any{
		"endOfSpeechOffsetInMilliseconds": heard * 1000 / opus.Rate,
		"startOfSpeechTimestamp":          u.ev.startedAt,
	})
	out.directive("InteractionModel", "RequestProcessingStarted", map[string]any{})

	a := t.Await()
	switch {
	case a.Err != nil:
		log.Printf("avs: req %d: %v", id, a.Err)
	case a.URL == "":
		log.Printf("avs: req %d: home assistant heard %d bytes and said nothing", id, len(a.Transcript))
	default:
		log.Printf("avs: req %d: home assistant heard %d bytes and answered with %d", id,
			len(a.Transcript), len(a.Speech))
	}
	if err := t.Close(); err != nil {
		log.Printf("avs: req %d: %v", id, err)
	}
	switch {
	case req.Context().Err() != nil:
	case a.URL != "":
		if !r.say(id, a.URL) {
			r.fail()
		}
	case a.Err != nil && a.Code != noSpeech:
		r.fail()
	}
	out.directive("InteractionModel", "RequestProcessingCompleted", map[string]any{})
	out.close()
}

func (r *Relay) fail() {
	if r.answer.Fail != nil {
		r.answer.Fail()
	}
}

func (r *Relay) decline(w http.ResponseWriter, id int64, u *upload, why string, tone bool) {
	go u.drain()
	log.Printf("avs: req %d: %s; the turn ends unanswered", id, untrustedlog.Cut(why))
	if tone {
		r.fail()
	}
	out := openReply(w, u.ev.dialogID)
	out.directive("InteractionModel", "RequestProcessingCompleted", map[string]any{})
	out.close()
}

func (r *Relay) say(id int64, url string) bool {
	if r.answer.Announce == nil {
		return false
	}
	done, err := r.answer.Announce(url)
	if err != nil {
		log.Printf("avs: req %d: playing the answer: %s", id, untrustedlog.Cut(err.Error()))
		return false
	}
	select {
	case <-done:
	case <-time.After(answerLimit):
		log.Printf("avs: req %d: the answer was still playing after %v", id, answerLimit)
	}
	return true
}

type replyWriter struct {
	w        http.ResponseWriter
	dialogID string
}

func openReply(w http.ResponseWriter, dialogID string) *replyWriter {
	w.Header().Set("Content-Type", `multipart/related; boundary=`+replyBoundary+`; type="application/json"`)
	w.WriteHeader(http.StatusOK)
	out := &replyWriter{w: w, dialogID: dialogID}
	out.write("--" + replyBoundary)
	return out
}

func (o *replyWriter) directive(namespace, name string, payload map[string]any) {
	header := map[string]string{"namespace": namespace, "name": name, "messageId": newID()}
	if o.dialogID != "" {
		header["dialogRequestId"] = o.dialogID
	}
	b, err := json.Marshal(map[string]any{"directive": map[string]any{"header": header, "payload": payload}})
	if err != nil {
		log.Printf("avs: %v", err)
		return
	}
	o.write(fmt.Sprintf("\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n%s\r\n--%s", b, replyBoundary))
}

func (o *replyWriter) close() { o.write("--\r\n") }

func (o *replyWriter) write(s string) {
	io.WriteString(o.w, s)
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
