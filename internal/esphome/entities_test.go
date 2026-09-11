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

func listedWithState(t *testing.T, s *Server) []map[int]pbField {
	t.Helper()
	var out []map[int]pbField
	for _, entity := range listed(t, s) {
		if entity[0].num == uint64(msgListEvent) {
			continue
		}
		out = append(out, entity)
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
		switch entity[0].num {
		case uint64(msgListBinarySensor), uint64(msgListSelect), uint64(msgListEvent),
			uint64(msgListSwitch):
			continue
		}
		if entity[0].num != uint64(msgListSensor) {
			t.Errorf("an entity of type %d is listed, and it is none of the kinds this "+
				"device has", entity[0].num)
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

func TestTheSpeakerIsListedAsABinarySensor(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

	found := 0
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListBinarySensor) || string(entity[1].data) != "speaker_playing" {
			continue
		}
		found++
		if uint32(entity[2].num) != s.keySound {
			t.Errorf("speaker_playing has key %d, want %d", entity[2].num, s.keySound)
		}
		if entity[2].wire != wireFixed32 {
			t.Errorf("speaker_playing sent its key as wire type %d, want fixed32 (%d)",
				entity[2].wire, wireFixed32)
		}
		if got := string(entity[3].data); got != "Speaker playing" {
			t.Errorf("speaker_playing is named %q", got)
		}
		if got := string(entity[5].data); got != "" {
			t.Errorf("speaker_playing has device_class %q, which renames its states away "+
				"from On and Off", got)
		}
		if entity[9].num != entityCategoryDiagnostic {
			t.Error("speaker_playing is not diagnostic; it would sit among the device's controls")
		}
		if got := string(entity[8].data); got != "mdi:speaker" {
			t.Errorf("speaker_playing has icon %q, want mdi:speaker", got)
		}
	}
	if found != 1 {
		t.Errorf("%d speaker_playing entities were listed, want 1", found)
	}
}

func TestTheEntityNumbersAreESPHomeS(t *testing.T) {
	for _, tt := range []struct {
		what string
		got  int
		want int
	}{
		{"ListEntitiesSensorResponse", msgListSensor, 16},
		{"ListEntitiesBinarySensorResponse", msgListBinarySensor, 12},
		{"ListEntitiesSelectResponse", msgListSelect, 52},
		{"ListEntitiesDoneResponse", msgListEntitiesDone, 19},
		{"SensorStateResponse", msgSensorState, 25},
		{"BinarySensorStateResponse", msgBinarySensorState, 21},
		{"SelectStateResponse", msgSelectState, 53},
		{"SelectCommandRequest", msgSelectCommand, 54},
		{"ListEntitiesSwitchResponse", msgListSwitch, 17},
		{"SwitchStateResponse", msgSwitchState, 26},
		{"SwitchCommandRequest", msgSwitchCommand, 33},
		{"SubscribeStatesRequest", msgSubscribeStates, 20},
		{"ENTITY_CATEGORY_CONFIG", entityCategoryConfig, 1},
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

func TestTheJackIsListedAsABinarySensor(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

	found := 0
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListBinarySensor) {
			continue
		}
		if string(entity[1].data) != "audio_jack" {
			continue
		}
		found++
		if uint32(entity[2].num) != s.keyJackOn {
			t.Errorf("audio_jack has key %d, want %d", entity[2].num, s.keyJackOn)
		}
		if entity[2].wire != wireFixed32 {
			t.Errorf("audio_jack sent its key as wire type %d, want fixed32 (%d)", entity[2].wire, wireFixed32)
		}
		if got := string(entity[3].data); got != "Audio jack" {
			t.Errorf("audio_jack is named %q, want \"Audio jack\"", got)
		}
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
		if entity[6].wire != wireVarint {
			t.Errorf("audio_jack sent field 6 as wire type %d, want varint (%d): a unit "+
				"borrowed from the sensor message would arrive here as a string",
				entity[6].wire, wireVarint)
		}
		if _, ok := entity[13]; ok {
			t.Error("audio_jack sets field 13, which is a sensor's entity_category and " +
				"unassigned on a binary sensor")
		}
		if got := string(entity[8].data); got != "" {
			t.Errorf("audio_jack carries icon %q, which replaces the plugged and unplugged "+
				"icons with one that never changes", got)
		}
	}
	if found != 1 {
		t.Errorf("%d audio_jack entities were listed, want 1", found)
	}
}

func TestTheMicrophoneIsListedAsASwitch(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

	found := 0
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListSwitch) {
			continue
		}
		found++
		if got := string(entity[1].data); got != "microphone_muted" {
			t.Errorf("the switch has object_id %q, want microphone_muted", got)
		}
		if uint32(entity[2].num) != s.keyMicMute {
			t.Errorf("microphone_muted has key %d, want %d", entity[2].num, s.keyMicMute)
		}
		if entity[2].wire != wireFixed32 {
			t.Errorf("microphone_muted sent its key as wire type %d, want fixed32 (%d)",
				entity[2].wire, wireFixed32)
		}
		if got := string(entity[3].data); got != "Microphone muted" {
			t.Errorf("microphone_muted is named %q", got)
		}
		if got := string(entity[5].data); got != "mdi:microphone-off" {
			t.Errorf("microphone_muted has icon %q, want mdi:microphone-off", got)
		}
		if entity[6].num != 0 {
			t.Error("microphone_muted claims an assumed state, and it reads the device")
		}
		if _, categorised := entity[8]; categorised {
			t.Errorf("microphone_muted carries entity_category %d, and it is a control "+
				"rather than a setting", entity[8].num)
		}
	}
	if found != 1 {
		t.Errorf("%d switch entities were listed, want 1", found)
	}
}
