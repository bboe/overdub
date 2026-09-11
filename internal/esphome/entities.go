package esphome

import "github.com/bboe/overdub/internal/device"

const (
	entityCategoryNone       = 0
	entityCategoryConfig     = 1
	entityCategoryDiagnostic = 2

	stateClassMeasurement     = 1
	stateClassTotalIncreasing = 2

	sensorAccuracyDecimals = 0

	volumeIcon = "mdi:volume-high"

	speakerIcon = "mdi:speaker"

	micIcon = "mdi:microphone-off"

	buttonModeIcon = "mdi:gesture-tap-button"

	adbIcon = "mdi:console-network"
)

var buttonModes = []string{"intercept", "monitor", "pass through"}

type EventType string

const (
	EventPressEnd       EventType = "press_end"
	EventMultiEnd       EventType = "multi_press_end"
	EventLongPressStart EventType = "long_press_start"
	EventLongPressEnd   EventType = "long_press_end"
)

var actionEvents = []EventType{
	EventPressEnd, EventMultiEnd, EventLongPressStart, EventLongPressEnd,
}

const esphomeVersion = "2026.8.0"

func (s *Server) deviceInfo() []byte {
	var msg pb
	msg.boolean(1, false) // uses_password
	msg.str(2, s.name)
	msg.str(3, s.mac)
	msg.str(4, esphomeVersion)
	msg.str(6, s.model)
	msg.str(12, "Amazon")
	msg.str(13, s.name) // friendly_name
	return msg.b
}

func (s *Server) listEntities(conn *conn) error {
	for _, b := range s.buttons {
		var action pb
		action.str(1, b.objectID)
		action.fixed32(2, b.keyEvent)
		action.str(3, b.name)
		action.boolean(6, false) // disabled_by_default
		action.u32(7, entityCategoryNone)
		action.str(8, "button") // device_class
		for _, eventType := range actionEvents {
			action.str(9, string(eventType))
		}
		if err := s.send(conn, msgListEvent, action.b); err != nil {
			return err
		}
	}

	var speaker pb
	speaker.str(1, "speaker")
	speaker.fixed32(2, s.keySpeaker)
	speaker.str(3, "Speaker")
	speaker.str(5, speakerIcon)
	speaker.boolean(6, false) // disabled_by_default
	speaker.u32(7, entityCategoryNone)
	speaker.u32(11, s.volumeFeatures()) // feature_flags
	if err := s.send(conn, msgListMediaPlayer, speaker.b); err != nil {
		return err
	}

	for _, sensor := range []struct {
		objectID    string
		key         uint32
		name        string
		unit        string
		deviceClass string
		stateClass  uint32
		icon        string
	}{
		{"uptime", s.keyUptime, "Uptime", "s", "duration", stateClassTotalIncreasing, ""},
		{"wifi_signal", s.keyWifi, "WiFi signal", "dBm", "signal_strength", stateClassMeasurement, ""},
		{"volume", s.keyVolume, "Volume", "%", "", stateClassMeasurement, volumeIcon},
		{"cpu_temperature", s.keyCPU, "CPU temperature", "°C", "temperature", stateClassMeasurement, ""},
		{"memory_available", s.keyMemory, "Memory available", "MiB", "data_size", stateClassMeasurement, ""},
		{"jack_volume", s.keyJack, "Jack volume", "%", "", stateClassMeasurement, volumeIcon},
	} {
		var entity pb
		entity.str(1, sensor.objectID)
		entity.fixed32(2, sensor.key)
		entity.str(3, sensor.name)
		entity.str(5, sensor.icon)
		entity.str(6, sensor.unit)
		entity.u32(7, sensorAccuracyDecimals)
		entity.boolean(8, false)
		entity.str(9, sensor.deviceClass)
		entity.u32(10, sensor.stateClass)
		entity.boolean(12, false)
		entity.u32(13, entityCategoryDiagnostic)
		if err := s.send(conn, msgListSensor, entity.b); err != nil {
			return err
		}
	}

	for _, sensor := range []struct {
		objectID    string
		key         uint32
		name        string
		deviceClass string
		icon        string
	}{
		{"audio_jack", s.keyJackOn, "Audio jack", "plug", ""},
		{"speaker_playing", s.keySound, "Speaker playing", "", speakerIcon},
	} {
		var entity pb
		entity.str(1, sensor.objectID)
		entity.fixed32(2, sensor.key)
		entity.str(3, sensor.name)
		entity.str(5, sensor.deviceClass)
		entity.boolean(6, false)
		entity.boolean(7, false)
		entity.str(8, sensor.icon)
		entity.u32(9, entityCategoryDiagnostic)
		if err := s.send(conn, msgListBinarySensor, entity.b); err != nil {
			return err
		}
	}

	var mic pb
	mic.str(1, "microphone_muted")
	mic.fixed32(2, s.keyMicMute)
	mic.str(3, "Microphone muted")
	mic.str(5, micIcon)
	mic.boolean(6, false)
	mic.boolean(7, false)
	if err := s.send(conn, msgListSwitch, mic.b); err != nil {
		return err
	}

	for _, b := range s.buttons {
		var mode pb
		mode.str(1, b.objectID+"_mode")
		mode.fixed32(2, b.keyMode)
		mode.str(3, b.name+" mode")
		mode.str(5, buttonModeIcon)
		for _, option := range buttonModes {
			mode.str(6, option)
		}
		mode.boolean(7, false) // disabled_by_default
		mode.u32(8, entityCategoryConfig)
		if err := s.send(conn, msgListSelect, mode.b); err != nil {
			return err
		}
	}

	var adb pb
	adb.str(1, "network_adb")
	adb.fixed32(2, s.keyADB)
	adb.str(3, "Network ADB")
	adb.str(5, adbIcon)
	adb.str(6, device.ADBOff.String()) // options
	adb.str(6, device.ADBInsecure.String())
	if s.adbSecureOK() {
		adb.str(6, device.ADBSecure.String())
	}
	adb.boolean(7, false) // disabled_by_default
	adb.u32(8, entityCategoryConfig)
	if err := s.send(conn, msgListSelect, adb.b); err != nil {
		return err
	}

	return s.send(conn, msgListEntitiesDone, nil)
}
