package esphome

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const commandWait = 2 * time.Second

func commandServer(t *testing.T) (*Server, chan string) {
	t.Helper()
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	sent := make(chan string, 4)
	s.UseCommand(func(text string) error {
		sent <- text
		return nil
	})
	return s, sent
}

func keyedText(key uint32, text string) []byte {
	var p pb
	p.fixed32(1, key)
	p.str(2, text)
	return p.b
}

func serviceCall(key uint32, text string) []byte {
	var arg pb
	arg.str(4, text)
	var p pb
	p.fixed32(1, key)
	p.sub(2, arg.b)
	return p.b
}

func TestATextCommandReachesAlexa(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
		keyedText(s.keyText, "what time is it")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sent:
		if got != "what time is it" {
			t.Errorf("alexa was asked to run %q", got)
		}
	case <-time.After(commandWait):
		t.Fatal("nothing reached alexa")
	}
}

func TestTheServiceReachesAlexaToo(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	if err := s.handle(&conn{sock: fakeAddr{}}, msgExecuteService,
		serviceCall(s.keyCommand, "turn on the lamp")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sent:
		if got != "turn on the lamp" {
			t.Errorf("alexa was asked to run %q", got)
		}
	case <-time.After(commandWait):
		t.Fatal("the service never reached alexa")
	}
}

func TestACommandForAnotherKeyIsNotRun(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
		keyedText(s.keyText+1, "unlock the front door")); err != nil {
		t.Fatal(err)
	}
	if err := s.handle(&conn{sock: fakeAddr{}}, msgExecuteService,
		serviceCall(s.keyCommand+1, "unlock the front door")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sent:
		t.Errorf("%q was run for a key that is not this entity's", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAnEmptyCommandIsNotRun(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	for _, text := range []string{"", "   ", "\t\n"} {
		if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
			keyedText(s.keyText, text)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-sent:
		t.Errorf("alexa was asked to run %q, which is nothing at all", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestACommandIsPublishedSoALaterSubscriberSeesIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
		keyedText(s.keyText, "set a timer for ten minutes")); err != nil {
		t.Fatal(err)
	}
	<-sent

	deadline := time.Now().Add(commandWait)
	for {
		s.mu.Lock()
		r, told := s.published[s.keyText]
		s.mu.Unlock()
		if told {
			if r.text != "set a timer for ten minutes" {
				t.Errorf("the entity holds %q", r.text)
			}
			if r.kind != kindText {
				t.Errorf("the command went out as kind %d, want kindText (%d)", r.kind, kindText)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the command was never published, so a subscriber arriving after it sees nothing")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestACommandsFailureIsNotCutToTheLengthOfAPeerString(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	done := make(chan struct{}, 1)
	const reason = "extract token: exit status 1: step: system context = android.app.ContextImpl@1; " +
		"step: map context = com.amazon.imp; accounts: 0"
	s.UseCommand(func(string) error {
		defer func() { done <- struct{}{} }()
		return errors.New(reason)
	})
	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
		keyedText(s.keyText, "what time is it")); err != nil {
		t.Fatal(err)
	}
	<-done

	deadline := time.Now().Add(commandWait)
	for !strings.Contains(out.String(), "accounts: 0") {
		if time.Now().After(deadline) {
			t.Fatalf("the reason was cut before the part that says why: %q", out.String())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestACommandThatFailedIsLoggedThroughThePeerLimit(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	done := make(chan struct{}, 1)
	s.UseCommand(func(string) error {
		defer func() { done <- struct{}{} }()
		return errors.New("cookie exchange: HTTP 401")
	})
	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
		keyedText(s.keyText, "what time is it")); err != nil {
		t.Fatal(err)
	}
	<-done

	deadline := time.Now().Add(commandWait)
	for !strings.Contains(out.String(), "HTTP 401") {
		if time.Now().After(deadline) {
			t.Fatalf("the failure was never logged: %q", out.String())
		}
		time.Sleep(time.Millisecond)
	}
	if s.untrustedLog.Written() == 0 {
		t.Error("the failure did not go through the peer rate limit, which a peer can cause")
	}
}

func TestTheCommandEntityIsListedOnlyWhenThereIsACredential(t *testing.T) {
	quiet := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	for _, entity := range listed(t, quiet) {
		if entity[0].num == uint64(msgListText) || entity[0].num == uint64(msgListService) {
			t.Error("a server with no way to run a command still offers one, so Home Assistant " +
				"shows a box that cannot work")
		}
	}

	s, _ := commandServer(t)
	text, service := 0, 0
	for _, entity := range listed(t, s) {
		switch entity[0].num {
		case uint64(msgListText):
			text++
			if got := string(entity[1].data); got != "alexa_command" {
				t.Errorf("the text entity is %q", got)
			}
			if entity[9].num != commandMaxLength {
				t.Errorf("max_length is %d, want %d", entity[9].num, commandMaxLength)
			}
			if entity[11].num != 0 {
				t.Error("the box is in PASSWORD mode, which hides what was typed into it")
			}
			if entity[7].num != entityCategoryConfig {
				t.Error("the command box is not a config entity")
			}
		case uint64(msgListService):
			service++
			if got := string(entity[1].data); got != "send_command" {
				t.Errorf("the service is %q", got)
			}
		}
	}
	if text != 1 || service != 1 {
		t.Errorf("%d text entities and %d services were listed, want 1 of each", text, service)
	}
}

func TestTheTextEntityNumbersAreESPHomeS(t *testing.T) {
	for _, tt := range []struct {
		what string
		got  int
		want int
	}{
		{"ListEntitiesTextResponse", msgListText, 97},
		{"TextStateResponse", msgTextState, 98},
		{"TextCommandRequest", msgTextCommand, 99},
		{"ListEntitiesServicesResponse", msgListService, 41},
		{"ExecuteServiceRequest", msgExecuteService, 42},
		{"SERVICE_ARG_STRING", serviceArgString, 3},
	} {
		if tt.got != tt.want {
			t.Errorf("%s is %d, want %d", tt.what, tt.got, tt.want)
		}
	}
}

func TestTheCommandBoxHasAStateBeforeAnybodyCommands(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, _ := commandServer(t)
	s.mu.Lock()
	r, told := s.published[s.keyText]
	s.mu.Unlock()
	if !told {
		t.Fatal("a fresh server publishes no state for the command box, so Home Assistant " +
			"draws it unavailable and greys out the one control that could give it a state")
	}
	if !r.ok || r.text != "" || r.kind != kindText {
		t.Errorf("the box starts at %+v; want an empty text state that is not missing", r)
	}
}

type closedAddr struct{ fakeAddr }

func (closedAddr) Close() error { return nil }

func TestWiringTheCommandLateRelistsForEverySubscriber(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	early := &conn{out: make(chan frame, sendQueue), sock: closedAddr{}, states: true}
	s.mu.Lock()
	s.conns[early] = struct{}{}
	s.mu.Unlock()

	s.UseCommand(func(string) error { return nil })

	s.mu.Lock()
	_, still := s.conns[early]
	s.mu.Unlock()
	if still {
		t.Error("a client that listed the entities before the box existed was left connected, " +
			"so it never learns the box is there")
	}
}

func TestCommandsQueueRatherThanReplaceEachOther(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	release := make(chan struct{})
	ran := make(chan string, 4)
	s.UseCommand(func(text string) error {
		<-release
		ran <- text
		return nil
	})

	for _, text := range []string{"set a timer for ten minutes", "turn on the lamp"} {
		if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
			keyedText(s.keyText, text)); err != nil {
			t.Fatal(err)
		}
	}
	close(release)

	var got []string
	for range 2 {
		select {
		case text := <-ran:
			got = append(got, text)
		case <-time.After(commandWait):
			t.Fatalf("only %v reached alexa; a command sent while another was in flight was "+
				"dropped, and the log says both ran", got)
		}
	}
	if got[0] != "set a timer for ten minutes" || got[1] != "turn on the lamp" {
		t.Errorf("commands ran as %v, want them in the order they were asked for", got)
	}
}

func TestAFloodOfCommandsIsBoundedAndSaysSo(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	release := make(chan struct{})
	s.UseCommand(func(string) error {
		<-release
		return nil
	})
	defer close(release)

	c := &conn{sock: fakeAddr{}}
	var told []string
	for i := range commandQueue + 4 {
		if err := s.handle(c, msgTextCommand,
			keyedText(s.keyText, fmt.Sprintf("command %d", i))); err != nil {
			t.Fatal(err)
		}
		told = append(told, c.noted)
	}

	s.mu.Lock()
	queued := len(s.cmdQueue)
	s.mu.Unlock()
	if queued > commandQueue {
		t.Errorf("%d commands are queued, want at most %d: a peer holding the key can "+
			"otherwise queue without bound", queued, commandQueue)
	}
	if !slices.ContainsFunc(told, func(said string) bool {
		return strings.Contains(said, "dropped rather than queued")
	}) {
		t.Errorf("the peer was told %q, and none of it says a command was dropped", told)
	}
}

func TestACommandLongerThanTheBoxIsRefused(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgTextCommand,
		keyedText(s.keyText, strings.Repeat("a", commandMaxLength+1))); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sent:
		t.Errorf("%d bytes were sent to alexa; the box advertises %d, and a peer holding the "+
			"key can otherwise put a whole frame into home assistant's recorder",
			len(got), commandMaxLength)
	case <-time.After(100 * time.Millisecond):
	}
	if !strings.Contains(c.noted, "the box takes") {
		t.Errorf("the peer was told %q, which does not say why nothing ran", c.noted)
	}

	if err := s.handle(c, msgTextCommand,
		keyedText(s.keyText, strings.Repeat("a", commandMaxLength))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sent:
	case <-time.After(commandWait):
		t.Error("a command of exactly the advertised length was refused as well")
	}
}

func TestClearingTheBoxClearsTheEntity(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
		keyedText(s.keyText, "what time is it")); err != nil {
		t.Fatal(err)
	}
	<-sent

	deadline := time.Now().Add(commandWait)
	for {
		s.mu.Lock()
		r := s.published[s.keyText]
		s.mu.Unlock()
		if r.text == "what time is it" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command was never published, so this test would prove nothing")
		}
		time.Sleep(time.Millisecond)
	}

	if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand, keyedText(s.keyText, "  ")); err != nil {
		t.Fatal(err)
	}
	for {
		s.mu.Lock()
		r := s.published[s.keyText]
		s.mu.Unlock()
		if r.text == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("emptying the box left the last command in it, so the card snaps back " +
				"to what was cleared")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-sent:
		t.Errorf("clearing the box asked alexa to run %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAClearQueuedBehindACommandStillWins(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	release := make(chan struct{})
	s.UseCommand(func(string) error {
		<-release
		return nil
	})

	for _, text := range []string{"what time is it", ""} {
		if err := s.handle(&conn{sock: fakeAddr{}}, msgTextCommand,
			keyedText(s.keyText, text)); err != nil {
			t.Fatal(err)
		}
	}
	close(release)

	deadline := time.Now().Add(commandWait)
	for {
		s.mu.Lock()
		r, told := s.published[s.keyText]
		queued := len(s.cmdQueue)
		s.mu.Unlock()
		if told && queued == 0 && r.text == "" {
			return
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			r = s.published[s.keyText]
			s.mu.Unlock()
			t.Fatalf("the box holds %q after being emptied; a clear sent while a command "+
				"was in flight was decided against a state the worker had not published yet",
				r.text)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTheLengthLimitCountsWhatTheBoxAdvertises(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s, sent := commandServer(t)
	c := &conn{sock: fakeAddr{}}
	// 200 three-byte runes: 600 bytes, and inside the 255 the listing advertises.
	if err := s.handle(c, msgTextCommand,
		keyedText(s.keyText, strings.Repeat("世", 200))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sent:
	case <-time.After(commandWait):
		t.Errorf("200 characters were refused: the box advertises %d, which Home Assistant "+
			"enforces as characters, so its own field would have let this through (%q)",
			commandMaxLength, c.noted)
	}
}
