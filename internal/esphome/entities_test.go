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
	}{
		"uptime":          {s.keyUptime, "Uptime", "s", "duration", stateClassTotalIncreasing},
		"wifi_signal":     {s.keyWifi, "WiFi signal", "dBm", "signal_strength", stateClassMeasurement},
		"volume":          {s.keyVolume, "Volume", "%", "", stateClassMeasurement},
		"cpu_temperature": {s.keyCPU, "CPU temperature", "°C", "temperature", stateClassMeasurement},
	}

	seen := map[string]bool{}
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListSensor) {
			t.Errorf("an entity of type %d is listed; this commit has only sensors", entity[0].num)
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
		{"ListEntitiesDoneResponse", msgListEntitiesDone, 19},
		{"SensorStateResponse", msgSensorState, 25},
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
