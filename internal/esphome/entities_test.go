package esphome

import "testing"

func listed(t *testing.T, s *Server) []map[int]pbField {
	t.Helper()
	c := &conn{out: make(chan frame, 32)}
	if err := s.listEntities(c); err != nil {
		t.Fatalf("listEntities: %v", err)
	}
	close(c.out)

	var out []map[int]pbField
	done := false
	for f := range c.out {
		if f.msgType == msgListEntitiesDone {
			done = true
			continue
		}
		// aioesphomeapi stops collecting at ListEntitiesDone, so anything listed
		// after it is an entity Home Assistant never sees.
		if done {
			t.Errorf("an entity of type %d was listed after ListEntitiesDone; "+
				"Home Assistant stops reading there", f.msgType)
		}
		fields := map[int]pbField{}
		if err := pbWalk(f.payload, func(field pbField) { fields[field.field] = field }); err != nil {
			t.Fatalf("entity %d did not parse: %v", f.msgType, err)
		}
		fields[0] = pbField{num: uint64(f.msgType)}
		out = append(out, fields)
	}
	if !done {
		t.Error("the listing never ended with ListEntitiesDone, so Home Assistant waits forever")
	}
	return out
}

func TestEverySensorIsListedTheWayHomeAssistantReadsIt(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

	want := map[string]struct {
		key         uint32
		name        string
		unit        string
		deviceClass string
		stateClass  uint64
		icon        string
	}{
		"uptime":           {s.keyUptime, "Uptime", "s", "duration", stateClassTotalIncreasing, ""},
		"wifi_signal":      {s.keyWifi, "WiFi signal", "dBm", "signal_strength", stateClassMeasurement, ""},
		"volume":           {s.keyVolume, "Volume", "%", "", stateClassMeasurement, volumeIcon},
		"cpu_temperature":  {s.keyCPU, "CPU temperature", "°C", "temperature", stateClassMeasurement, ""},
		"memory_available": {s.keyMemory, "Memory available", "MiB", "data_size", stateClassMeasurement, ""},
		"jack_volume":      {s.keyJack, "Jack volume", "%", "", stateClassMeasurement, volumeIcon},
	}

	seen := map[string]bool{}
	for _, entity := range listed(t, s) {
		// Binary sensors are listed under their own message type and checked by
		// TestTheJackIsListedAsABinarySensor: their fields are not these ones,
		// and reading them with this table would compare the wrong numbers.
		if entity[0].num == uint64(msgListBinarySensor) {
			continue
		}
		if entity[0].num != uint64(msgListSensor) {
			t.Errorf("an entity of type %d is listed, and it is neither sensor nor binary sensor", entity[0].num)
			continue
		}
		objectID := string(entity[1].data)
		expect, known := want[objectID]
		if !known {
			t.Errorf("a sensor called %q is listed and nothing expects it", objectID)
			continue
		}
		if uint32(entity[2].num) != expect.key {
			t.Errorf("%s has key %d, want %d", objectID, entity[2].num, expect.key)
		}
		// A key sent as a varint decodes to the same number here and to nothing
		// in Home Assistant, which skips the field and files the entity under
		// key zero along with every other one.
		if entity[2].wire != wireFixed32 {
			t.Errorf("%s sent its key as wire type %d, want fixed32 (%d)",
				objectID, entity[2].wire, wireFixed32)
		}
		if got := string(entity[3].data); got != expect.name {
			t.Errorf("%s is named %q, want %q", objectID, got, expect.name)
		}
		if got := string(entity[6].data); got != expect.unit {
			t.Errorf("%s has unit %q, want %q", objectID, got, expect.unit)
		}
		// A sensor with neither a device_class nor an icon is drawn as mdi:eye,
		// so the two volumes carry one. The rest take the icon their class
		// implies, which is why an icon set on them would be a regression
		// rather than a decoration.
		if got := string(entity[5].data); got != expect.icon {
			t.Errorf("%s has icon %q, want %q", objectID, got, expect.icon)
		}
		if expect.deviceClass == "" && expect.icon == "" {
			t.Errorf("%s has neither a device_class nor an icon, so Home Assistant draws it "+
				"as mdi:eye", objectID)
		}
		if got := string(entity[9].data); got != expect.deviceClass {
			t.Errorf("%s has device_class %q, want %q", objectID, got, expect.deviceClass)
		}
		if entity[10].num != expect.stateClass {
			t.Errorf("%s has state_class %d, want %d", objectID, entity[10].num, expect.stateClass)
		}
		if entity[7].num != sensorAccuracyDecimals {
			t.Errorf("%s reports %d decimals, want %d", objectID, entity[7].num, sensorAccuracyDecimals)
		}
		if entity[8].num != 0 {
			t.Errorf("%s sets force_update; Home Assistant would write a state every push, "+
				"unchanged or not", objectID)
		}
		if entity[12].num != 0 {
			t.Errorf("%s is disabled_by_default; it would not appear until somebody enabled it", objectID)
		}
		if entity[13].num != entityCategoryDiagnostic {
			t.Errorf("%s is not diagnostic; it would sit among the device's controls", objectID)
		}
		seen[objectID] = true
	}
	for objectID := range want {
		if !seen[objectID] {
			t.Errorf("%s was never listed", objectID)
		}
	}
}

// Numbers on the wire are ESPHome's, not ours. A test that compares one against
// the constant it was built from asserts nothing, and every one of these is
// silent when wrong: the entity is filed under the wrong heading, or Home
// Assistant never learns it exists at all.
func TestTheEntityNumbersAreESPHomeS(t *testing.T) {
	for _, tt := range []struct {
		what string
		got  int
		want int
	}{
		{"ListEntitiesSensorResponse", msgListSensor, 16},
		{"ListEntitiesBinarySensorResponse", msgListBinarySensor, 12},
		{"ListEntitiesDoneResponse", msgListEntitiesDone, 19},
		{"SensorStateResponse", msgSensorState, 25},
		{"BinarySensorStateResponse", msgBinarySensorState, 21},
		{"SubscribeStatesRequest", msgSubscribeStates, 20},
		{"ENTITY_CATEGORY_DIAGNOSTIC", entityCategoryDiagnostic, 2},
		{"STATE_CLASS_MEASUREMENT", stateClassMeasurement, 1},
		{"STATE_CLASS_TOTAL_INCREASING", stateClassTotalIncreasing, 2},
		{"accuracy_decimals for a whole number", sensorAccuracyDecimals, 0},
	} {
		if tt.got != tt.want {
			t.Errorf("%s is %d, want %d", tt.what, tt.got, tt.want)
		}
	}
}

// A binary sensor is a different message with different field numbers, not a
// sensor with a bool in it. Home Assistant reads the fields by number, so one
// borrowed from ListEntitiesSensor lands in the wrong place silently.
func TestTheJackIsListedAsABinarySensor(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

	found := 0
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListBinarySensor) {
			continue
		}
		found++
		if got := string(entity[1].data); got != "audio_jack" {
			t.Errorf("binary sensor object_id is %q, want audio_jack", got)
		}
		if uint32(entity[2].num) != s.keyJackOn {
			t.Errorf("audio_jack has key %d, want %d", entity[2].num, s.keyJackOn)
		}
		// A key sent as a varint decodes to the same number here and to nothing
		// in Home Assistant, which files every entity under key zero.
		if entity[2].wire != wireFixed32 {
			t.Errorf("audio_jack sent its key as wire type %d, want fixed32 (%d)", entity[2].wire, wireFixed32)
		}
		if got := string(entity[3].data); got != "Audio jack" {
			t.Errorf("audio_jack is named %q, want \"Audio jack\"", got)
		}
		// Field 5 on this message, where a sensor's device_class is field 9.
		if got := string(entity[5].data); got != "plug" {
			t.Errorf("audio_jack has device_class %q, want plug", got)
		}
		if entity[6].num != 0 {
			t.Error("audio_jack is a status binary sensor, which reports the connection rather than the jack")
		}
		if entity[7].num != 0 {
			t.Error("audio_jack is disabled_by_default; it would not appear until somebody enabled it")
		}
		if entity[9].num != entityCategoryDiagnostic {
			t.Error("audio_jack is not diagnostic; it would sit among the device's controls")
		}
		// The two messages disagree about what these numbers mean, so a field
		// copied from the sensor listing lands somewhere real and wrong. 6 is
		// unit_of_measurement on a sensor and is_status_binary_sensor here, so
		// the wire type separates them; 13 is the sensor's entity_category and
		// nothing at all here.
		if entity[6].wire != wireVarint {
			t.Errorf("audio_jack sent field 6 as wire type %d, want varint (%d): a unit "+
				"borrowed from the sensor message would arrive here as a string",
				entity[6].wire, wireVarint)
		}
		if _, ok := entity[13]; ok {
			t.Error("audio_jack sets field 13, which is a sensor's entity_category and " +
				"unassigned on a binary sensor")
		}
		// Home Assistant draws a binary sensor's two states with two icons of
		// the device_class's own -- mdi:power-plug-off unplugged and
		// mdi:power-plug plugged in. An icon of ours is used for both states
		// instead, so setting one here trades that pair for a picture that
		// never changes. Field 8, not the 5 the sensor message keeps it in.
		if got := string(entity[8].data); got != "" {
			t.Errorf("audio_jack carries icon %q, which replaces the plugged and unplugged "+
				"icons with one that never changes", got)
		}
	}
	if found != 1 {
		t.Errorf("%d binary sensors were listed, want 1", found)
	}
}
