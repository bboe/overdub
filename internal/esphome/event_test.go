package esphome

import (
	"reflect"
	"testing"
)

// The listing is the only place the event types are declared, and Home
// Assistant refuses one it was not told about. Repeated fields are why this
// walks the payload itself rather than going through listed(), which keeps one
// field per number.
func listedEvent(t *testing.T, s *Server) (map[int]pbField, []string) {
	t.Helper()
	c := &conn{out: make(chan frame, 32)}
	if err := s.listEntities(c); err != nil {
		t.Fatalf("listEntities: %v", err)
	}
	close(c.out)

	fields := map[int]pbField{}
	var eventTypes []string
	found := 0
	for f := range c.out {
		if f.msgType != msgListEvent {
			continue
		}
		found++
		if err := pbWalk(f.payload, func(field pbField) {
			if field.field == 9 {
				eventTypes = append(eventTypes, string(field.data))
				return
			}
			fields[field.field] = field
		}); err != nil {
			t.Fatalf("the event entity did not parse: %v", err)
		}
	}
	if found != 1 {
		t.Fatalf("%d event entities were listed, want 1", found)
	}
	return fields, eventTypes
}

// An event entity is a fifth message with a fifth set of field numbers, and
// every one of them is silent when wrong: 8 is device_class here and a binary
// sensor's icon, 9 is event_types here and device_class on a sensor.
func TestTheActionButtonIsListedAsAnEventEntity(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	entity, eventTypes := listedEvent(t, s)

	if got := string(entity[1].data); got != "action_button" {
		t.Errorf("the event entity has object_id %q, want action_button", got)
	}
	if uint32(entity[2].num) != s.keyAction {
		t.Errorf("action_button has key %d, want %d", entity[2].num, s.keyAction)
	}
	// A key sent as a varint decodes to the same number here and to nothing in
	// Home Assistant, which files every entity under key zero.
	if entity[2].wire != wireFixed32 {
		t.Errorf("action_button sent its key as wire type %d, want fixed32 (%d)",
			entity[2].wire, wireFixed32)
	}
	if got := string(entity[3].data); got != "Action button" {
		t.Errorf("action_button is named %q, want \"Action button\"", got)
	}
	if entity[6].num != 0 {
		t.Error("action_button is disabled_by_default; it would not appear until somebody enabled it")
	}
	// The one entity here that is neither a reading nor a control, so neither
	// category fits: a categorised entity is filed away from the device's
	// controls, and this is the device's whole point.
	if entity[7].num != entityCategoryNone {
		t.Errorf("action_button has entity_category %d, want none (%d)",
			entity[7].num, entityCategoryNone)
	}
	if got := string(entity[8].data); got != "button" {
		t.Errorf("action_button has device_class %q, want button", got)
	}
	// Home Assistant draws an event entity's icon from its device_class, so one
	// sent here would replace it and say no more than the class already does.
	if _, ok := entity[5]; ok {
		t.Error("action_button sends an icon, which replaces the one its device_class gives it")
	}

	// Spelled out rather than compared against actionEvents, which is what
	// builds the listing: a test that reads the same slice asserts nothing about
	// what Home Assistant is told.
	if want := []string{"press", "hold"}; !reflect.DeepEqual(eventTypes, want) {
		t.Errorf("action_button advertises %v, want %v", eventTypes, want)
	}
}

// The names on the wire, and the names main uses to pick between them. A type
// Home Assistant was not told about is dropped at its end, with the press lost
// and nothing here to say so.
func TestTheEventNamesAndNumbersAreESPHomeS(t *testing.T) {
	for _, tt := range []struct {
		what string
		got  int
		want int
	}{
		{"ListEntitiesEventResponse", msgListEvent, 107},
		{"EventResponse", msgEventState, 108},
	} {
		if tt.got != tt.want {
			t.Errorf("%s is %d, want %d", tt.what, tt.got, tt.want)
		}
	}
	if EventPress != "press" {
		t.Errorf("EventPress is %q, want press", EventPress)
	}
	if EventHold != "hold" {
		t.Errorf("EventHold is %q, want hold", EventHold)
	}
	if want := []EventType{EventPress, EventHold}; !reflect.DeepEqual(actionEvents, want) {
		t.Errorf("the listing advertises %v, want %v: an event type that is fired and not "+
			"listed is one Home Assistant drops", actionEvents, want)
	}
}

func TestFirePressReachesEverySubscriberAndNobodyElse(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	quiet := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}}
	loud := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true}
	s.mu.Lock()
	s.conns[quiet] = struct{}{}
	s.conns[loud] = struct{}{}
	s.mu.Unlock()

	s.FirePress(EventHold)

	if n := len(quiet.out); n != 0 {
		t.Errorf("a client that never subscribed was sent %d frames", n)
	}
	if len(loud.out) != 1 {
		t.Fatalf("a subscribed client was sent %d frames for one press, want 1", len(loud.out))
	}
	f := <-loud.out
	if f.msgType != msgEventState {
		t.Errorf("a press was sent as message %d, want EventResponse (%d)", f.msgType, msgEventState)
	}
	fields := map[int]pbField{}
	if err := pbWalk(f.payload, func(field pbField) { fields[field.field] = field }); err != nil {
		t.Fatalf("the event did not parse: %v", err)
	}
	if uint32(fields[1].num) != s.keyAction {
		t.Errorf("the event carries key %d, want %d", fields[1].num, s.keyAction)
	}
	if fields[1].wire != wireFixed32 {
		t.Errorf("the event sent its key as wire type %d, want fixed32 (%d)",
			fields[1].wire, wireFixed32)
	}
	if got := EventType(fields[2].data); got != EventHold {
		t.Errorf("the event carries type %q, want %q", got, EventHold)
	}
}

// A press is a moment rather than a value, so there is nothing for a client
// that arrives afterwards to be told. Publishing one would make the snapshot
// replay it, and a Home Assistant reconnecting would fire every automation
// hanging off a press nobody made.
func TestAPressIsNotPublished(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	s.FirePress(EventPress)

	s.mu.Lock()
	held := len(s.published)
	s.mu.Unlock()
	if held != 0 {
		t.Errorf("a press left %d readings in the published state, want 0", held)
	}

	late := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}}
	s.mu.Lock()
	s.conns[late] = struct{}{}
	s.mu.Unlock()
	if err := s.handle(late, msgSubscribeStates, nil); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Read by key rather than by message type, because the message a published
	// press comes back as is not the one it went out as. The snapshot encodes
	// by reading.kind, and a press stored as a reading would carry the zero
	// kind, so it would arrive as a sensor state carrying keyAction and never as
	// an EventResponse: a test watching for the event message alone cannot fail.
	close(late.out)
	for f := range late.out {
		var key uint32
		if err := pbWalk(f.payload, func(field pbField) {
			if field.field == 1 {
				key = uint32(field.num)
			}
		}); err != nil {
			t.Fatalf("a replayed frame did not parse: %v", err)
		}
		if key == s.keyAction {
			t.Errorf("a client that subscribed after a press was sent it, as message %d", f.msgType)
		}
	}
}
