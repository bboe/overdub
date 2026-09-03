package esphome

const (
	entityCategoryNone       = 0
	entityCategoryConfig     = 1
	entityCategoryDiagnostic = 2

	stateClassMeasurement     = 1
	stateClassTotalIncreasing = 2

	sensorAccuracyDecimals = 0

	// Home Assistant draws a sensor with no device_class as mdi:eye, and there
	// is no class for a volume percentage. Both volumes carry the same icon
	// because their names already say which is which.
	volumeIcon = "mdi:volume-high"

	speakerIcon = "mdi:speaker"

	// A switch renders a toggle control, so unlike a binary sensor its icon is
	// not what shows the state. That frees it to say which entity this is,
	// which is the job an icon has among a device's ten.
	captureIcon = "mdi:gesture-tap-button"
)

// What a press can turn out to be. Home Assistant refuses an event whose type
// the listing did not advertise, so the types it is told and the types FirePress
// is given have to be the same set. A type of its own is what holds that at the
// call site: actionEvents is both what the listing sends and the whole of what
// this package defines, so a caller reaching FirePress with anything else has
// to write the conversion that says so.
type EventType string

const (
	EventPress EventType = "press"
	EventHold  EventType = "hold"
)

var actionEvents = []EventType{EventPress, EventHold}

// What Home Assistant shows as the device's firmware version. Sent twice, in
// DeviceInfoResponse and in the mDNS TXT record, and the two have to agree:
// docs/architecture.md says what the number is for.
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
	// The button itself, and the only entity here that is not a reading or a
	// control: it reports a moment rather than a value, so it has no state and
	// no category. Home Assistant draws an event entity from its device_class,
	// so this carries no icon for the reason the jack does not.
	var action pb
	action.str(1, "action_button")
	action.fixed32(2, s.keyAction)
	action.str(3, "Action button")
	action.boolean(6, false) // disabled_by_default
	action.u32(7, entityCategoryNone)
	action.str(8, "button") // device_class
	for _, eventType := range actionEvents {
		action.str(9, string(eventType))
	}
	if err := s.send(conn, msgListEvent, action.b); err != nil {
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
		// plug is Home Assistant's own class for this: on is plugged in, off is
		// unplugged. The field numbers are ESPHome's ListEntitiesBinarySensor,
		// which is not the sensor message with a different name.
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

	// The one entity Home Assistant writes to. Config rather than diagnostic:
	// it changes what the device does rather than reporting what it is doing.
	var capture pb
	capture.str(1, "button_capture")
	capture.fixed32(2, s.keyCapture)
	capture.str(3, "Button capture")
	capture.str(5, captureIcon)
	capture.boolean(6, false) // assumed_state: this end knows, and is asked
	capture.boolean(7, false)
	capture.u32(8, entityCategoryConfig)
	if err := s.send(conn, msgListSwitch, capture.b); err != nil {
		return err
	}

	return s.send(conn, msgListEntitiesDone, nil)
}
