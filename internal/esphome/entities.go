package esphome

const (
	entityCategoryDiagnostic = 2

	stateClassMeasurement     = 1
	stateClassTotalIncreasing = 2

	sensorAccuracyDecimals = 0
)

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
	for _, sensor := range []struct {
		objectID    string
		key         uint32
		name        string
		unit        string
		deviceClass string
		stateClass  uint32
	}{
		{"uptime", s.keyUptime, "Uptime", "s", "duration", stateClassTotalIncreasing},
		{"wifi_signal", s.keyWifi, "WiFi signal", "dBm", "signal_strength", stateClassMeasurement},
		{"volume", s.keyVolume, "Volume", "%", "", stateClassMeasurement},
	} {
		var entity pb
		entity.str(1, sensor.objectID)
		entity.fixed32(2, sensor.key)
		entity.str(3, sensor.name)
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

	return s.send(conn, msgListEntitiesDone, nil)
}
