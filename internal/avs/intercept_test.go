package avs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/esphome"
	"github.com/bboe/overdub/internal/opus"
)

const uploadBoundary = "WhooHooZeerOoonie="

func eventPart(namespace, name, payload string) string {
	return `{"context":{"properties":[]},"event":{"header":{"namespace":"` + namespace +
		`","name":"` + name + `","messageId":"m1","dialogRequestId":"d1"},"payload":` + payload + `}}`
}

func recognize(wakeWordEnd string) string {
	return eventPart("SpeechRecognizer", "Recognize",
		`{"profile":"NEAR_FIELD","initiator":{"type":"WAKEWORD","payload":{"wakeWordIndices":`+
			`{"startIndexInSamples":8000,"endIndexInSamples":`+wakeWordEnd+`}}},`+
			`"startOfSpeechTimestamp":"123","format":"OPUS"}`)
}

func uploadPart(name, contentType, body string) string {
	return "\r\n--" + uploadBoundary + "\r\nContent-Disposition: form-data; name=\"" + name +
		"\"\r\nContent-Type: " + contentType + "\r\n\r\n" + body
}

func packets(n int) string {
	var b strings.Builder
	for i := range n {
		b.Write(bytes.Repeat([]byte{byte(i)}, opus.PacketBytes))
	}
	return b.String()
}

type fakeDecoder struct{}

func (fakeDecoder) Decode(packet []byte) ([]int16, error) {
	pcm := make([]int16, opus.FrameSamples)
	for i := range pcm {
		pcm[i] = int16(packet[0])
	}
	return pcm, nil
}

func (fakeDecoder) Close() {}

type fakeTurn struct {
	mu       sync.Mutex
	got      []int16
	ended    bool
	closed   bool
	hold     bool
	endAfter int

	heard    sync.Once
	speech   chan struct{}
	finished sync.Once
	done     chan struct{}
	answer   esphome.Answer
}

func newFakeTurn(endAfter int, answer esphome.Answer) *fakeTurn {
	return &fakeTurn{endAfter: endAfter, answer: answer,
		speech: make(chan struct{}), done: make(chan struct{})}
}

func (f *fakeTurn) Send(pcm []int16) error {
	f.mu.Lock()
	f.got = append(f.got, pcm...)
	n := len(f.got)
	f.mu.Unlock()
	if f.endAfter > 0 && n >= f.endAfter {
		f.hear()
	}
	return nil
}

func (f *fakeTurn) End() error {
	f.mu.Lock()
	f.ended = true
	hold := f.hold
	f.mu.Unlock()
	if !hold {
		f.finish()
	}
	return nil
}

func (f *fakeTurn) hear()                        { f.heard.Do(func() { close(f.speech) }) }
func (f *fakeTurn) finish()                      { f.hear(); f.finished.Do(func() { close(f.done) }) }
func (f *fakeTurn) SpeechEnded() <-chan struct{} { return f.speech }
func (f *fakeTurn) Done() <-chan struct{}        { return f.done }
func (f *fakeTurn) Await() esphome.Answer {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answer
}

func (f *fakeTurn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeTurn) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

type amazonLog struct {
	mu     sync.Mutex
	bodies []string
}

func (a *amazonLog) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		a.mu.Lock()
		a.bodies = append(a.bodies, string(body))
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

func (a *amazonLog) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.bodies...)
}

func intercepting(t *testing.T, turn *fakeTurn, startErr error) (*http.Client, *Relay, *amazonLog, *[]string) {
	t.Helper()
	amazon := &amazonLog{}
	client, r := proxied(t, amazon.handler())
	var played []string
	r.decoder = func() (decoder, error) { return fakeDecoder{}, nil }
	r.UseAnswerer(Answerer{
		Start: func(context.Context) (Turn, error) {
			if startErr != nil {
				return nil, startErr
			}
			return turn, nil
		},
		Announce: func(url string) (<-chan struct{}, error) {
			played = append(played, url)
			done := make(chan struct{})
			close(done)
			return done, nil
		},
		Fail: func() { played = append(played, "error tone") },
	})
	r.Intercept(true)
	return client, r, amazon, &played
}

func post(t *testing.T, client *http.Client, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://"+Endpoint+eventsPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+uploadBoundary)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type directive struct{ namespace, name, dialogID string }

func directives(t *testing.T, resp *http.Response) <-chan directive {
	t.Helper()
	boundary := boundaryOf(resp.Header.Get("Content-Type"))
	if boundary == "" {
		t.Fatalf("the reply is not multipart: %q", resp.Header.Get("Content-Type"))
	}
	out := make(chan directive, 8)
	w := newWatcher(boundary, func(part []byte) {
		var d struct {
			Directive struct {
				Header struct {
					Namespace, Name, DialogRequestID string
				}
			}
		}
		if json.Unmarshal(part, &d) == nil {
			h := d.Directive.Header
			out <- directive{h.Namespace, h.Name, h.DialogRequestID}
		}
	})
	go func() {
		defer close(out)
		io.Copy(w, resp.Body)
	}()
	return out
}

func next(t *testing.T, ds <-chan directive) directive {
	t.Helper()
	select {
	case d, ok := <-ds:
		if !ok {
			t.Fatal("the reply ended early")
		}
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no directive in 5 seconds")
	}
	return directive{}
}

func TestAnInterceptedTurnIsAnsweredWithoutAmazon(t *testing.T) {
	turn := newFakeTurn(10*opus.FrameSamples, esphome.Answer{Transcript: "what time is it", URL: "http://ha/answer.flac"})
	client, _, amazon, played := intercepting(t, turn, nil)

	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(uploadPart("metadata", "application/json", recognize("640")) +
			uploadPart("wakewordEngineMetadata", "application/octet-stream", "JSON_GZ\x00x") +
			uploadPart("audio", "application/octet-stream", "")))
		pw.Write([]byte(packets(13)))
	}()
	resp := post(t, client, pr)
	ds := directives(t, resp)
	for _, want := range []string{"SetEndOfSpeechOffset", "RequestProcessingStarted", "RequestProcessingCompleted"} {
		if d := next(t, ds); d.name != want || d.dialogID != "d1" {
			t.Fatalf("got %+v, want %s for dialog d1", d, want)
		}
	}
	pw.Close()

	if got := amazon.got(); len(got) != 0 {
		t.Errorf("Amazon got %d requests during an intercepted turn", len(got))
	}
	if len(*played) != 1 || (*played)[0] != "http://ha/answer.flac" {
		t.Errorf("played %q, want home assistant's answer", *played)
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if len(turn.got) < 10*opus.FrameSamples || turn.got[0] != 2 {
		t.Errorf("home assistant got %d samples starting at packet %d; the first 2 packets are the wake word",
			len(turn.got), turn.got[0])
	}
	if !turn.ended || !turn.closed {
		t.Errorf("ended %v, closed %v; the turn must be both", turn.ended, turn.closed)
	}
}

func TestCompletedWaitsForTheAnswerToEnd(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{URL: "http://ha/answer.flac"})
	amazon := &amazonLog{}
	client, r := proxied(t, amazon.handler())
	r.decoder = func() (decoder, error) { return fakeDecoder{}, nil }
	playing := make(chan struct{})
	r.UseAnswerer(Answerer{
		Start:    func(context.Context) (Turn, error) { return turn, nil },
		Announce: func(string) (<-chan struct{}, error) { return playing, nil },
	})
	r.Intercept(true)

	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte(uploadPart("metadata", "application/json", recognize("0")) +
		uploadPart("audio", "application/octet-stream", packets(8))))
	resp := post(t, client, pr)
	ds := directives(t, resp)
	next(t, ds)
	next(t, ds)
	select {
	case d := <-ds:
		t.Fatalf("got %+v while the answer was still playing", d)
	case <-time.After(300 * time.Millisecond):
	}
	turn.mu.Lock()
	closed := turn.closed
	turn.mu.Unlock()
	if !closed {
		t.Error("the turn was still open while the answer played, so a wake word then would go to Amazon")
	}
	close(playing)
	if d := next(t, ds); d.name != "RequestProcessingCompleted" {
		t.Errorf("got %+v after the answer, want RequestProcessingCompleted", d)
	}
}

func TestAFailedTurnNeverReachesAmazon(t *testing.T) {
	turn := newFakeTurn(0, esphome.Answer{Err: errors.New("no pipeline")})
	turn.finish()
	client, _, amazon, played := intercepting(t, turn, nil)

	body := uploadPart("metadata", "application/json", recognize("640")) +
		uploadPart("audio", "application/octet-stream", packets(6)) + "\r\n--" + uploadBoundary + "--\r\n"
	ds := directives(t, post(t, client, strings.NewReader(body)))
	var names []string
	for d := range ds {
		names = append(names, d.name)
	}
	if got := amazon.got(); len(got) != 0 {
		t.Errorf("Amazon got %d requests for a turn home assistant failed", len(got))
	}
	if len(names) == 0 || names[len(names)-1] != "RequestProcessingCompleted" ||
		strings.Join(*played, "|") != "error tone" {
		t.Errorf("got %q and played %q; want the turn closed with the error tone", names, *played)
	}
	if !turn.closed {
		t.Error("the failed turn was not closed")
	}
}

func TestWithNoSatelliteTheTurnEndsUnanswered(t *testing.T) {
	client, _, amazon, played := intercepting(t, nil, errors.New("not subscribed"))
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte(uploadPart("metadata", "application/json", recognize("640")) +
		uploadPart("audio", "application/octet-stream", packets(3))))
	ds := directives(t, post(t, client, pr))
	if d := next(t, ds); d.name != "RequestProcessingCompleted" || d.dialogID != "d1" {
		t.Errorf("got %+v while the upload was still open; want RequestProcessingCompleted, which ends the capture", d)
	}
	if got := amazon.got(); len(got) != 0 {
		t.Errorf("Amazon got %d requests with no satellite", len(got))
	}
	if strings.Join(*played, "|") != "error tone" {
		t.Errorf("played %q with no satellite, want the error tone", *played)
	}
}

func TestTheSideUploadsStayOnTheDevice(t *testing.T) {
	client, _, amazon, _ := intercepting(t, nil, errors.New("unused"))
	for _, name := range []string{"OneMicAudio", "DeviceAudioFeaturesReport"} {
		body := uploadPart("metadata", "application/json",
			eventPart("Alexa.AudioFrontEndController", name, `{"format":"OPUS"}`)) +
			uploadPart(name, "application/octet-stream", packets(4)) + "\r\n--" + uploadBoundary + "--\r\n"
		resp := post(t, client, strings.NewReader(body))
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s got %d, want 204", name, resp.StatusCode)
		}
	}
	if got := amazon.got(); len(got) != 0 {
		t.Errorf("Amazon got %d side uploads", len(got))
	}
}

func TestOtherEventsReachAmazonUnchanged(t *testing.T) {
	client, _, amazon, _ := intercepting(t, nil, errors.New("unused"))
	body := uploadPart("metadata", "application/json",
		eventPart("SpeechSynthesizer", "SpeechStarted", `{"token":"t"}`)) + "\r\n--" + uploadBoundary + "--\r\n"
	resp := post(t, client, strings.NewReader(body))
	io.ReadAll(resp.Body)
	if got := amazon.got(); len(got) != 1 || got[0] != body {
		t.Errorf("Amazon got %q, want the event unchanged", got)
	}
}

func TestTheTurnGoesToAmazonWhenInterceptIsOff(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{URL: "http://ha/a.flac"})
	client, r, amazon, played := intercepting(t, turn, nil)
	r.Intercept(false)
	body := uploadPart("metadata", "application/json", recognize("0")) +
		uploadPart("audio", "application/octet-stream", packets(3)) + "\r\n--" + uploadBoundary + "--\r\n"
	resp := post(t, client, strings.NewReader(body))
	io.ReadAll(resp.Body)
	if got := amazon.got(); len(got) != 1 || got[0] != body || len(*played) != 0 {
		t.Errorf("Amazon got %d requests and %d answers played; want the upload at Amazon", len(got), len(*played))
	}
}

func TestAlexasBoundaryIsReadThoughItEndsInAnEquals(t *testing.T) {
	if got := boundaryOf("multipart/form-data; boundary=" + uploadBoundary); got != uploadBoundary {
		t.Errorf("read %q, want %q", got, uploadBoundary)
	}
	if got := boundaryOf("text/plain; boundary=" + uploadBoundary); got != "" {
		t.Errorf("read %q from a type that is not multipart", got)
	}
}

func TestTheAudioPartIsPassedOnWhateverTheChunking(t *testing.T) {
	audio := packets(5) + "--" + uploadBoundary + "\r\n" + packets(2)
	body := uploadPart("metadata", "application/json", recognize("0")) +
		uploadPart("audio", "application/octet-stream", audio) + "\r\n--" + uploadBoundary + "--\r\n"
	for _, size := range []int{1, 7, 80, 1 << 20} {
		var got []byte
		w := newWatcher(uploadBoundary, func([]byte) {})
		w.onAudio = func(b []byte) { got = append(got, b...) }
		for i := 0; i < len(body); i += size {
			w.Write([]byte(body[i:min(i+size, len(body))]))
		}
		if string(got) != audio {
			t.Errorf("in %d-byte chunks the audio came out as %d bytes, want %d", size, len(got), len(audio))
		}
	}
}

type tones struct {
	mu sync.Mutex
	n  int
}

func (c *tones) play() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *tones) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func answering(t *testing.T, turn *fakeTurn, announce func(string) (<-chan struct{}, error)) (*http.Client, *tones) {
	t.Helper()
	return answeringWith(t, func(context.Context) (Turn, error) { return turn, nil }, announce)
}

func answeringWith(t *testing.T, start func(context.Context) (Turn, error),
	announce func(string) (<-chan struct{}, error)) (*http.Client, *tones) {
	t.Helper()
	client, r := proxied(t, (&amazonLog{}).handler())
	r.decoder = func() (decoder, error) { return fakeDecoder{}, nil }
	heard := &tones{}
	r.UseAnswerer(Answerer{Start: start, Announce: announce, Fail: heard.play})
	r.Intercept(true)
	return client, heard
}

func runTurn(t *testing.T, client *http.Client) []string {
	t.Helper()
	body := uploadPart("metadata", "application/json", recognize("0")) +
		uploadPart("audio", "application/octet-stream", packets(8)) + "\r\n--" + uploadBoundary + "--\r\n"
	var names []string
	for d := range directives(t, post(t, client, strings.NewReader(body))) {
		names = append(names, d.name)
	}
	return names
}

func TestAnAnswerThatCameWithAnErrorIsSpokenWithoutTheTone(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{URL: "http://ha/a.flac", Err: errors.New("left after the answer")})
	var played []string
	client, heard := answering(t, turn, func(url string) (<-chan struct{}, error) {
		played = append(played, url)
		done := make(chan struct{})
		close(done)
		return done, nil
	})
	runTurn(t, client)
	if len(played) != 1 || heard.count() != 0 {
		t.Errorf("played %q and %d tones; an answer that arrived is spoken, with no error tone", played, heard.count())
	}
}

func TestAnAnswerThatCannotPlayGetsTheTone(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{URL: "ftp://ha/a.flac"})
	client, heard := answering(t, turn, func(string) (<-chan struct{}, error) {
		return nil, errors.New("clip: the url is not http or https")
	})
	runTurn(t, client)
	if heard.count() != 1 {
		t.Errorf("played %d tones for an answer the clip player refused; want the error tone", heard.count())
	}
}

func TestATurnAlexaAbandonsGetsNoTone(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{})
	turn.hold = true
	client, heard := answeringWith(t, func(ctx context.Context) (Turn, error) {
		go func() {
			<-ctx.Done()
			turn.mu.Lock()
			turn.answer.Err = ctx.Err()
			turn.mu.Unlock()
			turn.finish()
		}()
		return turn, nil
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+Endpoint+eventsPath,
		strings.NewReader(uploadPart("metadata", "application/json", recognize("0"))+
			uploadPart("audio", "application/octet-stream", packets(8))+"\r\n--"+uploadBoundary+"--\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+uploadBoundary)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ds := directives(t, resp)
	next(t, ds)
	next(t, ds)
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for !turn.isClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !turn.isClosed() {
		t.Fatal("the handler never closed the turn")
	}
	time.Sleep(50 * time.Millisecond)
	if heard.count() != 0 {
		t.Errorf("played %d tones for a turn Alexa cancelled; want none", heard.count())
	}
}

func TestACommandWithNothingToSayIsSilent(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{Transcript: "turn on the light"})
	client, heard := answering(t, turn, nil)
	runTurn(t, client)
	if heard.count() != 0 {
		t.Errorf("played %d tones after a command that worked but said nothing; want none", heard.count())
	}
}

func TestAWakeWordDuringABusyTurnIsSilent(t *testing.T) {
	client, heard := answeringWith(t, func(context.Context) (Turn, error) {
		return nil, esphome.ErrTurnRunning
	}, nil)
	runTurn(t, client)
	if heard.count() != 0 {
		t.Errorf("played %d tones for a wake word while a turn was closing; want none", heard.count())
	}
}

func TestAWakeWordWithNothingAfterItIsSilent(t *testing.T) {
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{Code: "stt-no-text-recognized", Err: errors.New("No text recognized")})
	client, heard := answering(t, turn, nil)
	if names := runTurn(t, client); len(names) == 0 || names[len(names)-1] != "RequestProcessingCompleted" {
		t.Errorf("got %q, want the turn closed", names)
	}
	if heard.count() != 0 {
		t.Errorf("played %d tones when nobody spoke after the wake word; Amazon ends that turn silently", heard.count())
	}
}

func TestATurnHomeAssistantNeverFinishesGetsTheTone(t *testing.T) {
	saved := turnLimit
	turnLimit = 300 * time.Millisecond
	defer func() { turnLimit = saved }()
	turn := newFakeTurn(opus.FrameSamples, esphome.Answer{})
	turn.hold = true
	client, heard := answeringWith(t, func(ctx context.Context) (Turn, error) {
		go func() {
			<-ctx.Done()
			turn.mu.Lock()
			turn.answer.Err = ctx.Err()
			turn.mu.Unlock()
			turn.finish()
		}()
		return turn, nil
	}, nil)
	runTurn(t, client)
	if heard.count() != 1 {
		t.Errorf("played %d tones when home assistant never answered; want the error tone", heard.count())
	}
}
