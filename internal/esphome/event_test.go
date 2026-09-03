package esphome

import (
	"reflect"
	"testing"
	"time"
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

// An event entity is its own message with its own set of field numbers, and
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
	if want := []string{"press_end", "multi_press_end",
		"long_press_start", "long_press_end"}; !reflect.DeepEqual(eventTypes, want) {
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
	// ButtonEventType verbatim. A near miss files the event under no standard
	// trigger, and nothing reports that.
	for _, tt := range []struct {
		got  EventType
		want string
	}{
		{EventPressEnd, "press_end"},
		{EventMultiEnd, "multi_press_end"},
		{EventLongPressStart, "long_press_start"},
		{EventLongPressEnd, "long_press_end"},
	} {
		if string(tt.got) != tt.want {
			t.Errorf("an event type is %q, want %q", tt.got, tt.want)
		}
	}
	if want := []EventType{EventPressEnd, EventMultiEnd,
		EventLongPressStart, EventLongPressEnd}; !reflect.DeepEqual(actionEvents, want) {
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

	s.FirePress(EventLongPressEnd, 0, 742*time.Millisecond)

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
	if got := EventType(fields[2].data); got != EventLongPressEnd {
		t.Errorf("the event carries type %q, want %q", got, EventLongPressEnd)
	}
}

// A press is a moment rather than a value, so there is nothing for a client
// that arrives afterwards to be told. Publishing one would make the snapshot
// replay it, and a Home Assistant reconnecting would fire every automation
// hanging off a press nobody made.
func TestAPressIsNotPublished(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "00:00:5E:00:53:2A", nil)
	s.FirePress(EventPressEnd, 0, 0)

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

// Every value in a HomeassistantServiceMap is a string, so this reads the pairs
// back as the automation receives them: text, cast at the far end.
// Reads both map fields and keeps them apart: which one a key arrives in decides
// its type. Field 2 stays a string; Home Assistant parses a number out of 3.
func actionData(t *testing.T, payload []byte) (service string, isEvent bool, data, templated map[string]string) {
	t.Helper()
	data, templated = map[string]string{}, map[string]string{}
	pair := func(raw []byte) (string, string) {
		var key, value string
		if err := pbWalk(raw, func(f pbField) {
			switch f.field {
			case 1:
				key = string(f.data)
			case 2:
				value = string(f.data)
			}
		}); err != nil {
			t.Fatalf("a service map entry did not parse: %v", err)
		}
		return key, value
	}
	if err := pbWalk(payload, func(f pbField) {
		switch f.field {
		case 1:
			service = string(f.data)
		case 2:
			key, value := pair(f.data)
			data[key] = value
		case 3:
			key, value := pair(f.data)
			templated[key] = value
		case 5:
			isEvent = f.num != 0
		}
	}); err != nil {
		t.Fatalf("the service call did not parse: %v", err)
	}
	return service, isEvent, data, templated
}

// EventResponse carries a key and a type and nothing else, so the count rides a
// service call beside it. This is the shape the blueprint reads.
func TestThePressCountRidesAServiceCall(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	c := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true, services: true}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()

	s.FirePress(EventMultiEnd, 7, 0)

	if len(c.out) != 2 {
		t.Fatalf("a press sent %d frames to a client subscribed to both, want 2", len(c.out))
	}
	if f := <-c.out; f.msgType != msgEventState {
		t.Errorf("the first frame is message %d, want EventResponse (%d)", f.msgType, msgEventState)
	}
	f := <-c.out
	if f.msgType != msgHomeassistantAct {
		t.Fatalf("the count was sent as message %d, want HomeassistantServiceResponse (%d)",
			f.msgType, msgHomeassistantAct)
	}
	service, isEvent, data, templated := actionData(t, f.payload)

	// Home Assistant fires an is_event service call as a bus event only for its
	// own esphome domain and drops everything else, so the prefix is a rule
	// rather than a name somebody chose.
	if service != "esphome.overdub_pressed" {
		t.Errorf("the service call is named %q, want esphome.overdub_pressed", service)
	}
	if !isEvent {
		t.Error("the service call is not is_event, so Home Assistant calls a service instead of firing an event")
	}
	// The strings stay strings. device is the operator's -name, and Home
	// Assistant renders data_template, so a name with Jinja markers would be a
	// template this daemon asked it to run.
	for key, value := range map[string]string{"event_type": "multi_press_end", "device": "kitchen"} {
		if data[key] != value {
			t.Errorf("the service call carries %s=%q in data, want %q", key, data[key], value)
		}
		if _, ok := templated[key]; ok {
			t.Errorf("%s is sent in data_template, where Home Assistant would evaluate it", key)
		}
	}
	// The count is not a string by the time an automation sees it: field 3 is
	// rendered, and a rendered number parses back to a number.
	if templated["multi_press_count"] != "7" {
		t.Errorf("the count is %q in data_template, want %q", templated["multi_press_count"], "7")
	}
	if _, ok := data["multi_press_count"]; ok {
		t.Error("the count is sent in data, which reaches the automation as a string")
	}
}

// Separate subscriptions, and Home Assistant asks for both. A client that asked
// only for states gets the gesture and not the numbers.
func TestTheCountGoesOnlyToAClientThatAskedForServices(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	states := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true}
	services := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, services: true}
	s.mu.Lock()
	s.conns[states] = struct{}{}
	s.conns[services] = struct{}{}
	s.mu.Unlock()

	s.FirePress(EventMultiEnd, 2, 0)

	if len(states.out) != 1 {
		t.Fatalf("a states-only client was sent %d frames, want 1", len(states.out))
	}
	if f := <-states.out; f.msgType != msgEventState {
		t.Errorf("a states-only client was sent message %d, want the event (%d)",
			f.msgType, msgEventState)
	}
	if len(services.out) != 1 {
		t.Fatalf("a services-only client was sent %d frames, want 1", len(services.out))
	}
	if f := <-services.out; f.msgType != msgHomeassistantAct {
		t.Errorf("a services-only client was sent message %d, want the service call (%d)",
			f.msgType, msgHomeassistantAct)
	}
}

// The request that turns the count on, and the number it arrives as.
// SubscribeHomeassistantServicesRequest is 34 and the response is 35; a wrong
// number here is a message Home Assistant ignores, with no error at either end.
func TestSubscribingToServicesIsWhatTurnsTheCountOn(t *testing.T) {
	if msgSubscribeHAServ != 34 {
		t.Errorf("SubscribeHomeassistantServicesRequest is %d, want 34", msgSubscribeHAServ)
	}
	if msgHomeassistantAct != 35 {
		t.Errorf("HomeassistantServiceResponse is %d, want 35", msgHomeassistantAct)
	}

	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
	c := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()

	s.FirePress(EventPressEnd, 0, 0)
	if len(c.out) != 0 {
		t.Fatalf("a client that asked for nothing was sent %d frames, want 0", len(c.out))
	}
	if err := s.handle(c, msgSubscribeHAServ, nil); err != nil {
		t.Fatalf("handle: %v", err)
	}
	s.FirePress(EventPressEnd, 0, 0)
	if len(c.out) != 1 {
		t.Fatalf("a client that subscribed to services was sent %d frames, want 1", len(c.out))
	}
}

// Each extra key belongs to the gesture that has one. A count on a single press,
// or a duration on a run, is a number Home Assistant would draw as a
// measurement.
func TestOnlyAHoldCarriesItsDuration(t *testing.T) {
	for _, tt := range []struct {
		what      string
		eventType EventType
		count     int
		holdFor   time.Duration
		want      string
		wantCount string
	}{
		{"a hold", EventLongPressEnd, 0, 742 * time.Millisecond, "742", ""},
		{"a long hold", EventLongPressEnd, 0, 3 * time.Second, "3000", ""},
		{"a hold starting", EventLongPressStart, 0, 0, "", ""},
		{"a single press", EventPressEnd, 0, 0, "", ""},
		{"a run of seven", EventMultiEnd, 7, 0, "", "7"},
	} {
		s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)
		c := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, services: true}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.FirePress(tt.eventType, tt.count, tt.holdFor)
		if len(c.out) != 1 {
			t.Fatalf("%s sent %d frames, want 1", tt.what, len(c.out))
		}
		_, _, _, templated := actionData(t, (<-c.out).payload)
		for _, key := range []struct{ name, want string }{
			{"held_ms", tt.want},
			{"multi_press_count", tt.wantCount},
		} {
			got, present := templated[key.name]
			switch {
			case key.want == "" && present:
				t.Errorf("%s carries %s=%q, and has none to report", tt.what, key.name, got)
			case key.want != "" && got != key.want:
				t.Errorf("%s carries %s=%q, want %q", tt.what, key.name, got, key.want)
			}
		}
	}
}
