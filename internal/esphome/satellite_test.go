package esphome

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func satelliteServer(t *testing.T) (*Server, *conn) {
	t.Helper()
	s := testServer(t, testPSK(t))
	s.UseSatellite()
	c := listening(s)
	var subscribe pb
	subscribe.boolean(1, true) // subscribe
	handleWithin(t, s, c, msgSubscribeVoice, subscribe.b)
	return s, c
}

func handleWithin(t *testing.T, s *Server, c *conn, msgType int, payload []byte) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.handle(c, msgType, payload) }()
	select {
	case err := <-done:
		return err
	case <-time.After(testTimeout):
		t.Fatalf("message %d wedged the server lock, and every connection with it", msgType)
		return nil
	}
}

func voiceEvent(kind int, data ...string) []byte {
	var msg pb
	msg.u32(1, uint32(kind)) // event_type
	for i := 0; i+1 < len(data); i += 2 {
		var d pb
		d.str(1, data[i])   // name
		d.str(2, data[i+1]) // value
		msg.sub(2, d.b)     // data
	}
	return msg.b
}

func sent(t *testing.T, c *conn, msgType int) []byte {
	t.Helper()
	for {
		select {
		case f := <-c.out:
			if f.msgType == msgType {
				return f.payload
			}
		case <-time.After(testTimeout):
			t.Fatalf("message %d was never sent", msgType)
			return nil
		}
	}
}

func fieldOf(t *testing.T, msg []byte, want int) (pbField, bool) {
	t.Helper()
	var found pbField
	var ok bool
	if err := pbWalk(msg, func(f pbField) {
		if f.field == want {
			found, ok = f, true
		}
	}); err != nil {
		t.Fatal(err)
	}
	return found, ok
}

func awaitWithin(t *testing.T, turn *Turn) Answer {
	t.Helper()
	got := make(chan Answer, 1)
	go func() { got <- turn.Await() }()
	select {
	case a := <-got:
		return a
	case <-time.After(testTimeout):
		t.Fatal("the turn never finished")
		return Answer{}
	}
}

func TestOnlyASatelliteOffersVoice(t *testing.T) {
	s := testServer(t, testPSK(t))
	if _, ok := fieldOf(t, s.deviceInfo(), 17); ok {
		t.Error("a device that is not a satellite offers voice")
	}
	s.UseSatellite()
	flags, ok := fieldOf(t, s.deviceInfo(), 17)
	if !ok {
		t.Fatal("a satellite does not send voice_assistant_feature_flags")
	}
	if flags.num&voiceFeatureAssistant == 0 || flags.num&voiceFeatureAPIAudio == 0 {
		t.Errorf("flags %#x lack the assistant or API audio, so Home Assistant would drop the audio", flags.num)
	}
	if flags.num&voiceFeatureSpeaker != 0 {
		t.Errorf("flags %#x set the speaker, so the answer would come as WAV rather than the FLAC the clip player takes", flags.num)
	}
	if flags.num&voiceFeatureAnnounce != 0 {
		t.Errorf("flags %#x set announce, so Home Assistant would wait for a configuration nothing answers", flags.num)
	}
}

func TestATurnRunsFromStartToAnswer(t *testing.T) {
	s, c := satelliteServer(t)
	if !s.SatelliteReady() {
		t.Fatal("a subscribed satellite is not ready")
	}
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if start, ok := fieldOf(t, sent(t, c, msgVoiceRequest), 1); !ok || start.num != 1 {
		t.Fatal("the turn did not ask Home Assistant to start a pipeline")
	}
	if err := turn.Send([]int16{1, -2}); err != nil {
		t.Fatal(err)
	}
	audio, _ := fieldOf(t, sent(t, c, msgVoiceAudio), 1)
	if got := string(audio.data); got != "\x01\x00\xfe\xff" {
		t.Errorf("sent %q, want 16-bit little-endian samples", got)
	}

	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunStart))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventVADEnd))
	select {
	case <-turn.SpeechEnded():
	default:
		t.Fatal("the end of speech did not end the speech, so audio would stream until a cap")
	}
	if err := turn.End(); err != nil {
		t.Fatal(err)
	}
	if end, ok := fieldOf(t, sent(t, c, msgVoiceAudio), 2); !ok || end.num != 1 {
		t.Error("the end of the audio was not marked")
	}

	for _, ev := range [][]byte{
		voiceEvent(voiceEventSTTEnd, "text", "what time is it"),
		voiceEvent(voiceEventIntentEnd, "speech", "It is 2:00"),
		voiceEvent(voiceEventTTSStart, "text", "It is 2:00 PM"),
		voiceEvent(voiceEventTTSEnd, "url", "http://ha.invalid/api/tts_proxy/x.flac"),
		voiceEvent(voiceEventRunEnd),
	} {
		if err := handleWithin(t, s, c, msgVoiceEvent, ev); err != nil {
			t.Fatal(err)
		}
	}
	want := Answer{Transcript: "what time is it", Speech: "It is 2:00 PM", URL: "http://ha.invalid/api/tts_proxy/x.flac"}
	if got := awaitWithin(t, turn); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if err := turn.Close(); err != nil {
		t.Fatal(err)
	}
	if success, _ := fieldOf(t, sent(t, c, msgVoiceFinished), 1); success.num != 1 {
		t.Error("closing the turn did not report the answer finished, so Home Assistant stays responding")
	}
	if _, err := s.StartTurn(context.Background()); err != nil {
		t.Errorf("a closed turn still blocks the next one: %v", err)
	}
}

func TestOneTurnAtATime(t *testing.T) {
	s, _ := satelliteServer(t)
	if _, err := s.StartTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartTurn(context.Background()); err == nil {
		t.Error("a second turn started while the first was running")
	}
}

func TestNoTurnWithoutASubscriber(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.UseSatellite()
	if s.SatelliteReady() {
		t.Error("ready with nobody subscribed")
	}
	if _, err := s.StartTurn(context.Background()); !errors.Is(err, errNoSatellite) {
		t.Errorf("got %v, want %v", err, errNoSatellite)
	}
}

func TestAPipelineErrorEndsTheTurn(t *testing.T) {
	s, c := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunStart))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventError, "code", "stt-no-text-recognized", "message", "No text"))
	got := awaitWithin(t, turn)
	if got.Err == nil || !strings.Contains(got.Err.Error(), "stt-no-text-recognized") {
		t.Errorf("got %v, want the pipeline's error code", got.Err)
	}
	select {
	case <-turn.SpeechEnded():
	default:
		t.Error("a failed turn still waits for the end of speech")
	}
}

func TestARefusedPipelineEndsTheTurn(t *testing.T) {
	s, c := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var refused pb
	refused.boolean(2, true) // error
	handleWithin(t, s, c, msgVoiceResponse, refused.b)
	if got := awaitWithin(t, turn); got.Err == nil {
		t.Error("a refused pipeline left the turn without an error")
	}
}

func TestCancellingATurnAbortsThePipeline(t *testing.T) {
	s, c := satelliteServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := s.StartTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sent(t, c, msgVoiceRequest)
	cancel()
	if start, ok := fieldOf(t, sent(t, c, msgVoiceRequest), 1); !ok || start.num != 0 {
		t.Error("the cancelled turn did not ask Home Assistant to stop")
	}
	if got := awaitWithin(t, turn); !errors.Is(got.Err, context.Canceled) {
		t.Errorf("got %v, want %v", got.Err, context.Canceled)
	}
}

func TestEventsFromAnotherConnectionAreIgnored(t *testing.T) {
	s, _ := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := listening(s)
	handleWithin(t, s, other, msgVoiceEvent, voiceEvent(voiceEventRunEnd))
	var refused pb
	refused.boolean(2, true) // error
	handleWithin(t, s, other, msgVoiceResponse, refused.b)
	select {
	case <-turn.done:
		t.Error("a connection that holds no turn ended it")
	default:
	}
}

func TestANewSubscriberEndsTheOldOnesTurn(t *testing.T) {
	s, _ := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := listening(s)
	var subscribe pb
	subscribe.boolean(1, true) // subscribe
	handleWithin(t, s, other, msgSubscribeVoice, subscribe.b)
	if got := awaitWithin(t, turn); got.Err == nil {
		t.Error("the old subscriber's turn kept running")
	}
	if !s.SatelliteReady() {
		t.Error("the new subscriber is not the satellite")
	}
}

func TestAMalformedEventIsNotActedOn(t *testing.T) {
	s, c := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var msg pb
	msg.u32(1, voiceEventRunEnd)   // event_type
	msg.sub(2, []byte{0x0a, 0x7f}) // data: a name longer than the field
	if err := handleWithin(t, s, c, msgVoiceEvent, msg.b); err == nil {
		t.Error("a malformed event was accepted")
	}
	select {
	case <-turn.done:
		t.Error("a malformed event ended the turn")
	default:
	}
}

func TestHomeAssistantLeavingEndsTheTurn(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	s.UseSatellite()
	cl, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	var subscribe pb
	subscribe.boolean(1, true) // subscribe
	if err := cl.send(msgSubscribeVoice, subscribe.b); err != nil {
		t.Fatal(err)
	}
	handled(t, cl)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cl.conn.Close()
	if got := awaitWithin(t, turn); got.Err == nil {
		t.Error("the turn outlived the connection it ran on")
	}
	if s.SatelliteReady() {
		t.Error("a closed connection is still the satellite")
	}
	if err := turn.Send(make([]int16, 320)); err == nil {
		t.Error("audio was sent to a closed connection")
	}
	if err := turn.Close(); err == nil {
		t.Error("closing reported the answer finished to a closed connection")
	}
}

func TestTheTranscriptAlsoEndsTheSpeech(t *testing.T) {
	s, c := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunStart))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventSTTEnd, "text", "stop"))
	select {
	case <-turn.SpeechEnded():
	default:
		t.Error("the transcript did not end the speech, so audio would stream until a cap")
	}
}

func TestALateEventFromTheLastRunIsIgnored(t *testing.T) {
	s, c := satelliteServer(t)
	first, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunStart))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventError, "code", "x", "message", "y"))
	awaitWithin(t, first)
	first.Close()

	next, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventTTSEnd, "url", "http://ha.invalid/old.flac"))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventError, "code", "old", "message", "old"))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunEnd))
	select {
	case <-next.done:
		t.Fatal("the last run's ERROR or RUN_END ended the next turn")
	default:
	}
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunStart))
	handleWithin(t, s, c, msgVoiceEvent, voiceEvent(voiceEventRunEnd))
	if got := awaitWithin(t, next); got.URL != "" || got.Err != nil {
		t.Errorf("the next turn took %+v from the last run", got)
	}
}

func TestUnsubscribingEndsTheTurn(t *testing.T) {
	s, c := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var unsubscribe pb
	unsubscribe.boolean(1, false) // subscribe
	handleWithin(t, s, c, msgSubscribeVoice, unsubscribe.b)
	if got := awaitWithin(t, turn); got.Err == nil {
		t.Error("the turn outlived the subscription")
	}
	if s.SatelliteReady() {
		t.Error("an unsubscribed connection is still the satellite")
	}
}

func TestASupersededTurnSendsNothingToTheNewSubscriber(t *testing.T) {
	s, _ := satelliteServer(t)
	turn, err := s.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := listening(s)
	var subscribe pb
	subscribe.boolean(1, true) // subscribe
	handleWithin(t, s, other, msgSubscribeVoice, subscribe.b)
	if err := turn.Send(make([]int16, 320)); err == nil {
		t.Error("a superseded turn sent audio")
	}
	if len(other.out) != 0 {
		t.Error("the new subscriber got the old turn's audio")
	}
}
