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

// A select is its own message with its own set of field numbers, not a switch
// with more than two states. Every number here is silent
// when wrong: the entity lands under the wrong heading, or Home Assistant reads
// the field it finds there and shows something else.
// The select's options are a repeated field, which listed() cannot keep: it
// holds one pbField per number, so the last option would stand for all of them.
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
		// Only the action button's; the listing now carries one per button.
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
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	entity, options := listedSelect(t, s)

	if got := string(entity[1].data); got != "action_button_mode" {
		t.Errorf("the select has object_id %q, want action_button_mode", got)
	}
	if uint32(entity[2].num) != s.button("action_button").keyMode {
		t.Errorf("action_button_mode has key %d, want %d", entity[2].num, s.button("action_button").keyMode)
	}
	// A key sent as a varint decodes to the same number here and to nothing in
	// Home Assistant, which files every entity under key zero.
	if entity[2].wire != wireFixed32 {
		t.Errorf("action_button_mode sent its key as wire type %d, want fixed32 (%d)",
			entity[2].wire, wireFixed32)
	}
	// Home Assistant builds the entity id from the device name and this, and
	// the event entity is already called "Action button": the bare name would
	// put two entities of that name on the device page.
	if got := string(entity[3].data); got != "Action button mode" {
		t.Errorf("action_button_mode is named %q, want \"Action button mode\"", got)
	}
	// object_id is the slug of the name, as it is for every other entity here.
	if want := strings.ReplaceAll(strings.ToLower(string(entity[3].data)), " ", "_"); string(entity[1].data) != want {
		t.Errorf("object_id is %q and the name slugifies to %q; Home Assistant derives "+
			"the entity id from one of them", entity[1].data, want)
	}
	// Field 6 is the options, repeated, where 6 on a switch is assumed_state
	// and on a sensor is the unit. Home Assistant refuses an option the listing
	// did not name, so this is also the whole of what a SelectCommandRequest
	// may ask for. Spelled out rather than compared against buttonModes, which
	// is what builds the listing.
	if want := []string{"intercept", "monitor", "pass through"}; !reflect.DeepEqual(options, want) {
		t.Errorf("action_button_mode offers %v, want %v", options, want)
	}
	// Intercept first, because Home Assistant shows the options in the order it
	// is given them and the daemon exists to hold the button.
	if options[0] != "intercept" {
		t.Errorf("the first option is %q, want intercept", options[0])
	}
	if entity[7].num != 0 {
		t.Error("action_button_mode is disabled_by_default; it would not appear until somebody enabled it")
	}
	// Config rather than diagnostic: it changes what the device does. The
	// diagnostic ones report and this one is the only entity a peer writes.
	if entity[8].num != entityCategoryConfig {
		t.Errorf("action_button_mode has entity_category %d, want config (%d): it is a control, "+
			"not a reading", entity[8].num, entityCategoryConfig)
	}
	// Field 5 here, where a binary sensor keeps its device_class. The jack
	// refuses an icon because a binary sensor's icon pair is the only thing
	// drawing its state; a select is rendered with a dropdown, so the pair it
	// gives up was redundant and the icon can identify it instead.
	if got := string(entity[5].data); got != buttonModeIcon {
		t.Errorf("action_button_mode has icon %q, want %q", got, buttonModeIcon)
	}
}

// selectCommand is what aioesphomeapi sends for select.select_option.
func selectCommand(key uint32, choice string) []byte {
	var p pb
	p.fixed32(1, key)
	p.str(2, choice)
	return p.b
}

// A select state carries a word where a sensor carries a number, so the drains
// below cannot use sensorReading.
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

// The reader and the writer wired to one flag, the way serve.go wires them to
// the button, plus a way to wait for the write.
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
		// Never blocking, because the real one never does: setMode is called
		// with the server lock held, so a fake that blocked would wedge the
		// accept path and every other connection rather than fail its own test.
		select {
		case b.changed <- choice:
		default:
		}
	})
	return b
}

// The whole point of the entity, end to end: Home Assistant asks, the button
// hears about it, and the new state comes back as a select state rather than
// as nothing. Nothing is published from handle itself -- publish takes the
// server lock and handle holds it -- so the state only arrives if the wake and
// the poll behind it are both wired.
func TestChoosingPassThroughHandsTheButtonBackAndReportsIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	// The change reaches Home Assistant through a wake, and that wake is gated:
	// at the shipped second this would wait one out behind the poll's own first
	// read. TestTogglingCannotOutrunTheWakeGap covers the gate itself.
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
	// Parked an hour out, so nothing but the wake can deliver the new state.
	go s.PollSensors(time.Hour)

	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Drain until the button reports the mode it ships in.
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
	// The subscribe queued a wake of its own before the poll started, and that
	// wake alone would deliver a second publish carrying whatever the state had
	// become. Spent here, so what follows can only be the select's own wake:
	// without this, deleting that wake entirely leaves this test green.
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
	// handle only parks the line on the connection; the read loop is what
	// writes it. Without this, deleting that drain leaves the suite green and
	// the only record of who moved the button is silently lost.
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

// The key is how Home Assistant says which entity it means, and this device
// has one select per button. A command that ignored the key would take
// every select.select_option on the device as this one.
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

// pbWalk visits the fields it read before it failed, so acting on a message
// that did not parse means acting on whatever was scraped out of it -- here, a
// key and a state that may both be halves of something else. The same rule
// HelloRequest is under, and this is the second message with a payload to read.
func TestAMalformedSelectCommandIsNotActedOn(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	// A well-formed key and state, then a length that runs off the end.
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

// A wake costs the sensor poll a read of /proc, and a peer holding the key can
// send commands as fast as it likes. Choosing the mode the button is already in
// is not a change and buys neither.
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

// The log is where an unresponsive button is told apart from one Home Assistant
// let go of, and it is the only place that distinction is recorded. Written by
// the read loop rather than under the server lock, which is why handle leaves
// it on the connection.
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

// A server built without a button reports the shipped default and moves
// nothing, rather than tracking a mode no read loop is reading. The select
// exists on every server because the listing is what Home Assistant caches, and
// an entity that came and went would need a reconfigure to reappear.
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

// The select is the one thing a peer can ask for repeatedly on one connection
// that used to reach the device. Subscribing wakes the polls only on a
// connection's first request, so spamming that needed a new connection each
// time and the eight slots bounded it; a select command needs neither, and
// cycling the modes changes the state every time so the no-op guard turns
// none of them away. Without the gap, one peer buys a procfs read and a push to
// every subscriber per message it sends.
func TestTogglingCannotOutrunTheWakeGap(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	wireFakeButton(s)
	s.wakeGap = time.Hour // nothing a wake asks for may be served

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
	// The poll's own first turn, which is not a wake and is always served.
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
	// Long enough that a poll willing to serve a wake would have done so
	// twenty times over.
	time.Sleep(200 * time.Millisecond)

	if got := count(); got != before {
		t.Errorf("twenty commands drew %d readings of the device, want 0: a peer holding "+
			"the key sets the poll's rate", got-before)
	}
	// The commands did land, or this passes on a server that ignored them.
	if want := buttonModes[19%len(buttonModes)]; s.button("action_button").mode() != want {
		t.Errorf("the last command asked for %q and the button is in %q", want, s.button("action_button").mode())
	}
}

// The reader and the writer are set together or not at all. Two exported fields
// let a caller wire the writer and leave the reader at the default, which is a
// select that really moves the button and reports the shipped mode for ever:
// Home Assistant's dropdown snaps back on every poll, and nothing says why.
func TestTheButtonIsWiredAsOnePiece(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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
	// The reader follows the writer, which is the whole point: the state
	// readTicked publishes has to be the one the button is actually in.
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

// A select can be asked for a word rather than a bit, so unlike a switch it can
// be asked for something that is not a state at all. Home Assistant only offers
// what the listing named, so anything else is a peer making one up -- and the
// button must not be moved to a mode nothing can name.
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
		// Refusing is worth a line: it is a peer sending something Home
		// Assistant would not have, and the only sign of it.
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

// Every button gets both entities, or there is one that reports without being
// configurable, or one configurable that reports nothing. The mute button is
// the second, so this is also what says the listing stopped being singular.
func TestEveryButtonIsListedAsAnEventAndASelect(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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
	// Spelled out rather than read from s.buttons, which is what builds the
	// listing: a test reading the same slice asserts nothing about the wire.
	for _, objectID := range []string{"action_button", "mute_button"} {
		if !events[objectID] {
			t.Errorf("%s has no event entity, so its presses reach nobody", objectID)
		}
		if !selects[objectID+"_mode"] {
			t.Errorf("%s has no mode select, so nothing can change what it does", objectID)
		}
	}
	// The device's own selects are not buttons and are not counted here: what
	// this holds is that no button was listed with half a pair.
	delete(selects, "network_adb")
	if len(events) != 2 || len(selects) != 2 {
		t.Errorf("listed %d event entities and %d button selects, want 2 of each", len(events), len(selects))
	}
}

// A server nobody has wired reports a mode rather than an empty string, and it
// is a placeholder rather than any button's shipped mode: the caller owns the
// keys, so it owns what each starts in. main_test.go holds the real ones.
func TestAnUnwiredButtonReportsAnOfferedMode(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
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

// One button's select must not move another's. The keys are separate hashes and
// setModeLocked picks by them, so a command naming mute cannot reach the action
// button -- which is the whole of what keeps two selects from being one.
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
