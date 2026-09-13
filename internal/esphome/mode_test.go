package esphome

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func listedSelect(t *testing.T, s *Server) (map[int]pbField, []string) {
	t.Helper()
	c := &conn{out: make(chan frame, 32)}
	if err := s.listEntities(c); err != nil {
		t.Fatalf("listEntities: %v", err)
	}
	close(c.out)

	fields := map[int]pbField{}
	var options []string
	found := 0
	for f := range c.out {
		if f.msgType != msgListSelect {
			continue
		}
		if !bytes.Contains(f.payload, []byte("action_button_mode")) {
			continue
		}
		found++
		if err := pbWalk(f.payload, func(field pbField) {
			if field.field == 6 {
				options = append(options, string(field.data))
				return
			}
			fields[field.field] = field
		}); err != nil {
			t.Fatalf("the select entity did not parse: %v", err)
		}
	}
	if found != 1 {
		t.Fatalf("%d select entities were listed, want 1", found)
	}
	return fields, options
}

func TestTheButtonModeIsListedTheWayHomeAssistantReadsIt(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	entity, options := listedSelect(t, s)

	if got := string(entity[1].data); got != "action_button_mode" {
		t.Errorf("the select has object_id %q, want action_button_mode", got)
	}
	if uint32(entity[2].num) != s.button("action_button").keyMode {
		t.Errorf("action_button_mode has key %d, want %d", entity[2].num, s.button("action_button").keyMode)
	}
	if entity[2].wire != wireFixed32 {
		t.Errorf("action_button_mode sent its key as wire type %d, want fixed32 (%d)",
			entity[2].wire, wireFixed32)
	}
	if got := string(entity[3].data); got != "Action button mode" {
		t.Errorf("action_button_mode is named %q, want \"Action button mode\"", got)
	}
	if want := strings.ReplaceAll(strings.ToLower(string(entity[3].data)), " ", "_"); string(entity[1].data) != want {
		t.Errorf("object_id is %q and the name slugifies to %q; Home Assistant derives "+
			"the entity id from one of them", entity[1].data, want)
	}
	if want := []string{"intercept", "monitor", "pass through"}; !reflect.DeepEqual(options, want) {
		t.Errorf("action_button_mode offers %v, want %v", options, want)
	}
	if options[0] != "intercept" {
		t.Errorf("the first option is %q, want intercept", options[0])
	}
	if entity[7].num != 0 {
		t.Error("action_button_mode is disabled_by_default; it would not appear until somebody enabled it")
	}
	if entity[8].num != entityCategoryConfig {
		t.Errorf("action_button_mode has entity_category %d, want config (%d): it is a control, "+
			"not a reading", entity[8].num, entityCategoryConfig)
	}
	if got := string(entity[5].data); got != buttonModeIcon {
		t.Errorf("action_button_mode has icon %q, want %q", got, buttonModeIcon)
	}
}

func selectCommand(key uint32, choice string) []byte {
	var p pb
	p.fixed32(1, key)
	p.str(2, choice)
	return p.b
}

func selectReading(t *testing.T, payload []byte) (uint32, string) {
	t.Helper()
	var key uint32
	var choice string
	if err := pbWalk(payload, func(f pbField) {
		switch f.field {
		case 1:
			key = uint32(f.num)
		case 2:
			choice = string(f.data)
		}
	}); err != nil {
		t.Fatalf("a select state did not parse: %v", err)
	}
	return key, choice
}

type fakeButton struct {
	mu      sync.Mutex
	mode    string
	changed chan string
}

func wireFakeButton(s *Server) *fakeButton {
	b := &fakeButton{mode: buttonModes[0], changed: make(chan string, 8)}
	s.UseButton("action_button", func() string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.mode
	}, func(choice string) {
		b.mu.Lock()
		b.mode = choice
		b.mu.Unlock()
		select {
		case b.changed <- choice:
		default:
		}
	})
	return b
}

func TestChoosingPassThroughHandsTheButtonBackAndReportsIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.wakeGap = 10 * time.Millisecond
	stubSensors(s)
	button := wireFakeButton(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	go s.PollSensors(time.Hour)

	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the select never reported its starting state: %v", err)
		}
		if msgType != msgSelectState {
			continue
		}
		key, choice := selectReading(t, payload)
		if key != s.button("action_button").keyMode {
			continue
		}
		if choice != "intercept" {
			t.Fatalf("action_button_mode started at %q, want intercept: the daemon holds the button", choice)
		}
		break
	}
	time.Sleep(20 * s.wakeGap)

	if err := c.send(msgSelectCommand, selectCommand(s.button("action_button").keyMode, "pass through")); err != nil {
		t.Fatal(err)
	}
	select {
	case choice := <-button.changed:
		if choice != "pass through" {
			t.Fatalf("select.select_option asked the button for %q", choice)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("select.select_option never reached the button")
	}
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the button was handed back and Home Assistant was never told: %v", err)
		}
		if msgType != msgSelectState {
			continue
		}
		key, choice := selectReading(t, payload)
		if key != s.button("action_button").keyMode {
			continue
		}
		if choice != "pass through" {
			t.Fatalf("action_button_mode came back as %q after select.select_option", choice)
		}
		break
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		if strings.Contains(out.String(), "set action_button to pass through") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mode change was never logged; the log says %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestASelectCommandForAnotherEntityIsIgnored(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if err := s.handle(c, msgSelectCommand, selectCommand(s.keySound, "pass through")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	select {
	case choice := <-button.changed:
		t.Errorf("a command addressed to the speaker sensor set the mode to %q", choice)
	default:
	}
}

func TestAMalformedSelectCommandIsNotActedOn(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	payload := append(selectCommand(s.button("action_button").keyMode, "pass through"), 0x1a, 0x7f, 'x')
	if err := s.handle(c, msgSelectCommand, payload); err == nil {
		t.Error("a SelectCommandRequest that did not parse was accepted")
	}
	select {
	case choice := <-button.changed:
		t.Errorf("a malformed command set the mode to %q", choice)
	default:
	}
}

func TestASelectCommandThatChangesNothingCostsNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, "intercept")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	select {
	case choice := <-button.changed:
		t.Errorf("the button was told to be %q, which it already was", choice)
	default:
	}
	select {
	case <-s.sensorWake:
		t.Error("a command that changed nothing woke the sensor poll")
	default:
	}
	if c.noted != "" {
		t.Errorf("a command that changed nothing wrote %q to the log", c.noted)
	}
}

func TestTheHandoverIsCarriedOutOfTheLockToBeLogged(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, "pass through")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if c.noted == "" {
		t.Fatal("the button changed hands and nothing was left for the log")
	}
	if out.String() != "" {
		t.Errorf("handle wrote %q with the server lock held", out.String())
	}
}

func TestAServerWithNoButtonMovesNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if s.button("action_button").mode() != "intercept" {
		t.Errorf("a server with no button reports mode %q, want intercept", s.button("action_button").mode())
	}
	if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, "pass through")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if s.button("action_button").mode() != "intercept" {
		t.Error("a server with no button reported the mode changing anyway")
	}
	if c.noted != "" {
		t.Errorf("a server with no button logged %q about a handover that did not happen", c.noted)
	}
}

func TestTogglingCannotOutrunTheWakeGap(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	wireFakeButton(s)
	s.wakeGap = time.Hour

	var mu sync.Mutex
	reads := 0
	s.uptime = func() (float32, bool) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return 1234, true
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}

	go s.PollSensors(time.Hour)
	for deadline := time.Now().Add(3 * time.Second); count() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the sensor poll never took its first reading, so this proves nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := count()

	c := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}}
	for i := 0; i < 20; i++ {
		if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, buttonModes[i%len(buttonModes)])); err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)

	if got := count(); got != before {
		t.Errorf("twenty commands drew %d readings of the device, want 0: a peer holding "+
			"the key sets the poll's rate", got-before)
	}
	if want := buttonModes[19%len(buttonModes)]; s.button("action_button").mode() != want {
		t.Errorf("the last command asked for %q and the button is in %q", want, s.button("action_button").mode())
	}
}

func TestTheButtonIsWiredAsOnePiece(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if s.button("action_button").mode == nil {
		t.Fatal("a server with no button has no reader, so readTicked panics")
	}
	if s.button("action_button").setMode != nil {
		t.Error("a server with no button carries a writer, so the select moves something")
	}

	held := "intercept"
	s.UseButton("action_button", func() string { return held }, func(choice string) { held = choice })
	b := s.button("action_button")
	if b.mode == nil || b.setMode == nil {
		t.Fatal("UseButton left half the wiring unset")
	}
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}
	if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, "pass through")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if held != "pass through" {
		t.Error("the writer was never called")
	}
	if s.button("action_button").mode() != "pass through" {
		t.Error("the reader still reports the old mode after the writer changed it")
	}
}

func TestAModeThatWasNeverOfferedIsRefused(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	for _, choice := range []string{"", "INTERCEPT", "captured", "pass  through", "monitor "} {
		s := testServer(t, testPSK(t))
		button := wireFakeButton(s)
		c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

		if err := s.handle(c, msgSelectCommand, selectCommand(s.button("action_button").keyMode, choice)); err != nil {
			t.Fatalf("handle(%q): %v", choice, err)
		}
		select {
		case got := <-button.changed:
			t.Errorf("mode %q was never offered and moved the button to %q", choice, got)
		default:
		}
		if c.noted == "" {
			t.Errorf("mode %q was refused and nothing was left for the log", choice)
		}
		select {
		case <-s.sensorWake:
			t.Errorf("mode %q was refused and still woke the sensor poll", choice)
		default:
		}
	}
}

func TestEveryButtonIsListedAsAnEventAndASelect(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	c := &conn{out: make(chan frame, 64)}
	if err := s.listEntities(c); err != nil {
		t.Fatalf("listEntities: %v", err)
	}
	close(c.out)

	events, selects := map[string]bool{}, map[string]bool{}
	for f := range c.out {
		var objectID string
		if err := pbWalk(f.payload, func(field pbField) {
			if field.field == 1 && objectID == "" {
				objectID = string(field.data)
			}
		}); err != nil {
			t.Fatalf("an entity did not parse: %v", err)
		}
		switch f.msgType {
		case msgListEvent:
			events[objectID] = true
		case msgListSelect:
			selects[objectID] = true
		}
	}
	for _, objectID := range []string{"action_button", "mute_button"} {
		if !events[objectID] {
			t.Errorf("%s has no event entity, so its presses reach nobody", objectID)
		}
		if !selects[objectID+"_mode"] {
			t.Errorf("%s has no mode select, so nothing can change what it does", objectID)
		}
	}
	delete(selects, "network_adb")
	if len(events) != 2 || len(selects) != 2 {
		t.Errorf("listed %d event entities and %d button selects, want 2 of each", len(events), len(selects))
	}
}

func TestAnUnwiredButtonReportsAnOfferedMode(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	for _, objectID := range []string{"action_button", "mute_button"} {
		b := s.button(objectID)
		if b == nil {
			t.Fatalf("%s is not a button on this server", objectID)
		}
		if !slices.Contains(buttonModes, b.mode()) {
			t.Errorf("%s reports %q, which is not a mode the listing offers", objectID, b.mode())
		}
	}
}

func TestOneButtonsSelectDoesNotMoveAnother(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	moved := map[string]string{}
	for _, objectID := range []string{"action_button", "mute_button"} {
		s.UseButton(objectID,
			func() string { return "intercept" },
			func(choice string) { moved[objectID] = choice })
	}
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	key := s.button("mute_button").keyMode
	if err := s.handle(c, msgSelectCommand, selectCommand(key, "pass through")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if moved["action_button"] != "" {
		t.Errorf("a command for the mute button moved the action button to %q", moved["action_button"])
	}
	if moved["mute_button"] != "pass through" {
		t.Errorf("the mute button was moved to %q, want pass through", moved["mute_button"])
	}
}
