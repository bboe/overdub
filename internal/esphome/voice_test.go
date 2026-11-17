package esphome

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

type fakeVoice struct {
	mode  string
	reply string
	set   []string
}

func wireFakeVoice(s *Server) *fakeVoice {
	v := &fakeVoice{mode: "pass through"}
	s.UseVoice([]string{"monitor", "pass through"},
		func() string { return v.mode },
		func(choice string) {
			v.mode = choice
			v.set = append(v.set, choice)
			if choice != "monitor" {
				v.reply = ""
			}
		},
		func() string { return v.reply })
	return v
}

func listedEntity(t *testing.T, s *Server, msgType int, objectID string) (map[int]pbField, []string, bool) {
	t.Helper()
	c := &conn{out: make(chan frame, 64)}
	if err := s.listEntities(c); err != nil {
		t.Fatalf("listEntities: %v", err)
	}
	close(c.out)
	for f := range c.out {
		if f.msgType != msgType || !bytes.Contains(f.payload, []byte(objectID)) {
			continue
		}
		fields := map[int]pbField{}
		var options []string
		if err := pbWalk(f.payload, func(field pbField) {
			if msgType == msgListSelect && field.field == 6 {
				options = append(options, string(field.data))
				return
			}
			fields[field.field] = field
		}); err != nil {
			t.Fatalf("%s did not parse: %v", objectID, err)
		}
		return fields, options, true
	}
	return nil, nil, false
}

func TestTheVoiceModeAndAlexasReplyAreListed(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if _, _, ok := listedEntity(t, s, msgListSelect, "voice_mode"); ok {
		t.Error("a server with no relay wired listed a voice mode that would move nothing")
	}
	wireFakeVoice(s)

	mode, options, ok := listedEntity(t, s, msgListSelect, "voice_mode")
	if !ok {
		t.Fatal("no voice_mode select was listed")
	}
	if uint32(mode[2].num) != s.keyVoice || string(mode[3].data) != "Voice mode" {
		t.Errorf("voice_mode has key %d and name %q", mode[2].num, mode[3].data)
	}
	if want := []string{"monitor", "pass through"}; !reflect.DeepEqual(options, want) {
		t.Errorf("voice_mode offers %v, want %v", options, want)
	}
	if mode[8].num != entityCategoryConfig {
		t.Errorf("voice_mode has entity_category %d, want config", mode[8].num)
	}

	reply, _, ok := listedEntity(t, s, msgListTextSensor, "alexa_reply")
	if !ok {
		t.Fatal("no alexa_reply text sensor was listed")
	}
	if uint32(reply[2].num) != s.keyReply || string(reply[3].data) != "Alexa reply" {
		t.Errorf("alexa_reply has key %d and name %q", reply[2].num, reply[3].data)
	}
}

func TestChoosingAVoiceModeSetsItAndWakesThePoll(t *testing.T) {
	s := testServer(t, testPSK(t))
	v := wireFakeVoice(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if err := s.handle(c, msgSelectCommand, selectCommand(s.keyVoice, "monitor")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.set, []string{"monitor"}) {
		t.Errorf("the mode was set %v times, want [monitor]", v.set)
	}
	select {
	case <-s.sensorWake:
	default:
		t.Error("the new mode waits for the next tick to reach Home Assistant")
	}
	if err := s.handle(c, msgSelectCommand, selectCommand(s.keyVoice, "monitor")); err != nil {
		t.Fatal(err)
	}
	if len(v.set) != 1 {
		t.Errorf("choosing the mode it already had set it again: %v", v.set)
	}
}

func TestAVoiceModeThatWasNeverOfferedIsRefused(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	for _, choice := range []string{"", "intercept", "Monitor", "pass  through"} {
		s := testServer(t, testPSK(t))
		v := wireFakeVoice(s)
		c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}
		if err := s.handle(c, msgSelectCommand, selectCommand(s.keyVoice, choice)); err != nil {
			t.Fatal(err)
		}
		if len(v.set) != 0 || c.noted == "" {
			t.Errorf("mode %q was never offered: set %v, noted %q", choice, v.set, c.noted)
		}
	}
}

func TestAButtonSelectDoesNotMoveTheVoiceMode(t *testing.T) {
	s := testServer(t, testPSK(t))
	v := wireFakeVoice(s)
	wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}
	if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, "monitor")); err != nil {
		t.Fatal(err)
	}
	if len(v.set) != 0 {
		t.Errorf("a button's select moved the voice mode: %v", v.set)
	}
}

func TestTheTickCarriesTheVoiceModeButNotTheReply(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	v := wireFakeVoice(s)
	v.mode, v.reply = "monitor", "It's 9:41."
	var mode *reading
	for _, r := range s.readTicked() {
		switch r.key {
		case s.keyVoice:
			mode = &r
		case s.keyReply:
			t.Errorf("the tick read the reply %q; a tick that read it before a new answer was published would put the old one back", r.text)
		}
	}
	if mode == nil || mode.kind != kindSelect || mode.text != "monitor" {
		t.Errorf("the voice mode read %+v, want the select at monitor", mode)
	}
}

func TestANewSubscriberGetsTheReplyHeldAtWiring(t *testing.T) {
	s := testServer(t, testPSK(t))
	wireFakeVoice(s)
	for _, r := range s.snapshot() {
		if r.key == s.keyReply {
			if !r.ok || r.kind != kindTextSensor || r.text != "" {
				t.Errorf("the reply was held as %+v, want an empty text sensor", r)
			}
			return
		}
	}
	t.Error("a subscriber gets no reply state until Alexa first answers")
}

func TestLeavingMonitorPublishesTheEmptiedReply(t *testing.T) {
	s := testServer(t, testPSK(t))
	v := wireFakeVoice(s)
	v.mode, v.reply = "monitor", "It's 9:41."
	s.NoteReply()
	c := listening(s)
	if err := s.handle(&conn{out: make(chan frame, 8), sock: fakeAddr{}}, msgSelectCommand,
		selectCommand(s.keyVoice, "pass through")); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-c.out:
		if f.msgType != msgTextSensorState || bytes.Contains(f.payload, []byte("9:41")) {
			t.Errorf("sent message %d, %q; want the reply's text sensor, emptied", f.msgType, f.payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass through left Alexa's last answer in the sensor")
	}
}

func TestAReplyIsSentAloneAndAtOnce(t *testing.T) {
	s := testServer(t, testPSK(t))
	v := wireFakeVoice(s)
	c := listening(s)
	v.reply = "The time is 8:46 AM."
	s.NoteReply()
	select {
	case f := <-c.out:
		if f.msgType != msgTextSensorState || !bytes.Contains(f.payload, []byte(v.reply)) {
			t.Errorf("sent message %d, %q; want the reply's text sensor state", f.msgType, f.payload)
		}
	default:
		t.Fatal("the reply waited for the sensor poll")
	}
	if len(c.out) != 0 {
		t.Errorf("%d more messages went with the reply; it is one string", len(c.out))
	}
	select {
	case <-s.sensorWake:
		t.Error("the reply woke the whole sensor poll, adb's firewall check included")
	default:
	}
}
