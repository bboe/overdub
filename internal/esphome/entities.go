package esphome

const entityCategoryDiagnostic = 2

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
	var uptime pb
	uptime.str(1, "uptime")
	uptime.fixed32(2, s.keyUptime)
	uptime.str(3, "Uptime")
	uptime.str(6, "s")
	uptime.u32(7, 0) // accuracy_decimals
	uptime.boolean(8, false)
	uptime.str(9, "duration")
	uptime.u32(10, 2) // state_class: total_increasing
	uptime.boolean(12, false)
	uptime.u32(13, entityCategoryDiagnostic)
	if err := s.send(conn, msgListSensor, uptime.b); err != nil {
		return err
	}

	return s.send(conn, msgListEntitiesDone, nil)
}
