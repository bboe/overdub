package esphome

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// A switch is its own message with its own set of field numbers, not a binary
// sensor Home Assistant happens to let you press. Every number here is silent
// when wrong: the entity lands under the wrong heading, or Home Assistant reads
// the field it finds there and shows something else.
func TestTheButtonSwitchIsListedTheWayHomeAssistantReadsIt(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

	found := 0
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListSwitch) {
			continue
		}
		found++
		if got := string(entity[1].data); got != "button_capture" {
			t.Errorf("the switch has object_id %q, want button_capture", got)
		}
		if uint32(entity[2].num) != s.keyCapture {
			t.Errorf("button_capture has key %d, want %d", entity[2].num, s.keyCapture)
		}
		// A key sent as a varint decodes to the same number here and to nothing
		// in Home Assistant, which files every entity under key zero.
		if entity[2].wire != wireFixed32 {
			t.Errorf("button_capture sent its key as wire type %d, want fixed32 (%d)",
				entity[2].wire, wireFixed32)
		}
		if got := string(entity[3].data); got != "Button capture" {
			t.Errorf("button_capture is named %q, want \"Button capture\"", got)
		}
		// This end knows what it did with the button and is asked before it
		// changes, so Home Assistant can draw one toggle rather than the
		// separate on and off buttons assumed_state gives an unreadable device.
		if entity[6].num != 0 {
			t.Error("button_capture sets assumed_state, and its state is not assumed")
		}
		if entity[7].num != 0 {
			t.Error("button_capture is disabled_by_default; it would not appear until somebody enabled it")
		}
		// Config rather than diagnostic: it changes what the device does. The
		// diagnostic ones report and this one is the only entity a peer writes.
		if entity[8].num != entityCategoryConfig {
			t.Errorf("button_capture has entity_category %d, want config (%d): it is a control, "+
				"not a reading", entity[8].num, entityCategoryConfig)
		}
		// Field 5 here, where a binary sensor keeps its device_class. The jack
		// refuses an icon because a binary sensor's icon pair is the only thing
		// drawing its state; a switch is rendered with a toggle control, so the
		// pair it gives up was redundant and the icon can identify it instead.
		if got := string(entity[5].data); got != captureIcon {
			t.Errorf("button_capture has icon %q, want %q: without one Home Assistant "+
				"draws a generic toggle that says nothing about which entity it is",
				got, captureIcon)
		}
	}
	if found != 1 {
		t.Errorf("%d button_capture entities were listed, want 1", found)
	}
}

// switchCommand is what aioesphomeapi sends for switch.turn_on and .turn_off.
func switchCommand(key uint32, on bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(2, on)
	return p.b
}

// The reader and the writer wired to one flag, the way serve.go wires them to
// the button, plus a way to wait for the write.
type fakeButton struct {
	mu       sync.Mutex
	captured bool
	changed  chan bool
}

func wireFakeButton(s *Server) *fakeButton {
	b := &fakeButton{captured: true, changed: make(chan bool, 8)}
	s.UseButton(func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.captured
	}, func(on bool) {
		b.mu.Lock()
		b.captured = on
		b.mu.Unlock()
		// Never blocking, because the real one never does: Capture is called
		// with the server lock held, so a fake that blocked would wedge the
		// accept path and every other connection rather than fail its own test.
		select {
		case b.changed <- on:
		default:
		}
	})
	return b
}

// The whole point of the entity, end to end: Home Assistant asks, the button
// hears about it, and the new state comes back as a switch state rather than
// as nothing. Nothing is published from handle itself -- publish takes the
// server lock and handle holds it -- so the state only arrives if the wake and
// the poll behind it are both wired.
func TestTurningTheSwitchOffHandsTheButtonBackAndReportsIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	// The toggle reaches Home Assistant through a wake, and that wake is gated:
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
	// Drain until the button is reported captured, which is where it starts.
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the switch never reported its starting state: %v", err)
		}
		if key, v, _ := sensorReading(t, msgType, payload); key == s.keyCapture {
			if msgType != msgSwitchState {
				t.Fatalf("button_capture arrived as message %d, want SwitchStateResponse (%d)",
					msgType, msgSwitchState)
			}
			if v != 1 {
				t.Fatalf("button_capture started at %v, want 1: the daemon holds the button", v)
			}
			break
		}
	}
	// The subscribe queued a wake of its own before the poll started, and that
	// wake alone would deliver a second publish carrying whatever the state had
	// become. Spent here, so what follows can only be the switch's own wake:
	// without this, deleting that wake entirely leaves this test green.
	time.Sleep(20 * s.wakeGap)

	if err := c.send(msgSwitchCommand, switchCommand(s.keyCapture, false)); err != nil {
		t.Fatal(err)
	}
	select {
	case on := <-button.changed:
		if on {
			t.Fatalf("switch.turn_off asked the button for %v", on)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("switch.turn_off never reached the button")
	}
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the button was handed back and Home Assistant was never told: %v", err)
		}
		key, v, _ := sensorReading(t, msgType, payload)
		if key != s.keyCapture {
			continue
		}
		if msgType != msgSwitchState {
			t.Fatalf("button_capture arrived as message %d, want SwitchStateResponse (%d)",
				msgType, msgSwitchState)
		}
		if v != 0 {
			t.Fatalf("button_capture came back as %v after switch.turn_off", v)
		}
		break
	}
	// handle only parks the line on the connection; the read loop is what
	// writes it. Without this, deleting that drain leaves the suite green and
	// the only record of who moved the button is silently lost.
	for deadline := time.Now().Add(3 * time.Second); ; {
		if strings.Contains(out.String(), "handed the action button back to Alexa") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the handover was never logged; the log says %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The key is how Home Assistant says which entity it means, and this device
// has exactly one switch. A command that ignored the key would take
// every switch.turn_off on the device as this one.
func TestASwitchCommandForAnotherEntityIsIgnored(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keySound, false)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	select {
	case on := <-button.changed:
		t.Errorf("a command addressed to the speaker sensor set capture to %v", on)
	default:
	}
}

// pbWalk visits the fields it read before it failed, so acting on a message
// that did not parse means acting on whatever was scraped out of it -- here, a
// key and a state that may both be halves of something else. The same rule
// HelloRequest is under, and this is the second message with a payload to read.
func TestAMalformedSwitchCommandIsNotActedOn(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	// A well-formed key and state, then a length that runs off the end.
	payload := append(switchCommand(s.keyCapture, false), 0x1a, 0x7f, 'x')
	if err := s.handle(c, msgSwitchCommand, payload); err == nil {
		t.Error("a SwitchCommandRequest that did not parse was accepted")
	}
	select {
	case on := <-button.changed:
		t.Errorf("a malformed command set capture to %v", on)
	default:
	}
}

// A wake costs the sensor poll a read of /proc, and a peer holding the key can
// send commands as fast as it likes. Turning on a switch that is already on is
// not a change and buys neither.
func TestASwitchCommandThatChangesNothingCostsNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	button := wireFakeButton(s)
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keyCapture, true)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	select {
	case on := <-button.changed:
		t.Errorf("the button was told to be %v, which it already was", on)
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

	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keyCapture, false)); err != nil {
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
// nothing, rather than tracking a flag no read loop is reading. The switch
// exists on every server because the listing is what Home Assistant caches, and
// an entity that came and went would need a reconfigure to reappear.
func TestAServerWithNoButtonMovesNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}

	if !s.captured() {
		t.Error("a server with no button reports the action button as Alexa's")
	}
	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keyCapture, false)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !s.captured() {
		t.Error("a server with no button reported the state changing anyway")
	}
	if c.noted != "" {
		t.Errorf("a server with no button logged %q about a handover that did not happen", c.noted)
	}
}

// The switch is the one thing a peer can ask for repeatedly on one connection
// that used to reach the device. Subscribing wakes the polls only on a
// connection's first request, so spamming that needed a new connection each
// time and the eight slots bounded it; a switch command needs neither, and
// alternating on and off changes the state every time so the no-op guard turns
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
		if err := s.handle(c, msgSwitchCommand, switchCommand(s.keyCapture, i%2 == 0)); err != nil {
			t.Fatalf("toggle %d: %v", i, err)
		}
	}
	// Long enough that a poll willing to serve a wake would have done so
	// twenty times over.
	time.Sleep(200 * time.Millisecond)

	if got := count(); got != before {
		t.Errorf("twenty toggles drew %d readings of the device, want 0: a peer holding "+
			"the key sets the poll's rate", got-before)
	}
	// The toggles did land, or this passes on a server that ignored them.
	if s.captured() {
		t.Error("the last toggle asked for the button to be released and it was not")
	}
}

// The reader and the writer are set together or not at all. Two exported fields
// let a caller wire the writer and leave the reader at the default, which is a
// switch that really moves the button and reports it captured for ever: Home
// Assistant's toggle snaps back on every poll, and nothing says why.
func TestTheButtonIsWiredAsOnePiece(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	if s.captured == nil {
		t.Fatal("a server with no button has no reader, so readTicked panics")
	}
	if s.capture != nil {
		t.Error("a server with no button carries a writer, so the switch moves something")
	}

	held := true
	s.UseButton(func() bool { return held }, func(on bool) { held = on })
	if s.captured == nil || s.capture == nil {
		t.Fatal("UseButton left half the wiring unset")
	}
	// The reader follows the writer, which is the whole point: the state
	// readTicked publishes has to be the one the button is actually in.
	c := &conn{out: make(chan frame, 8), sock: fakeAddr{}}
	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keyCapture, false)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if held {
		t.Error("the writer was never called")
	}
	if s.captured() {
		t.Error("the reader still reports the button captured after the writer released it")
	}
}
