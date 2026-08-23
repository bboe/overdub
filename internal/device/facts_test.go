package device

import "testing"

func TestParseUptimeTakesTheFirstField(t *testing.T) {
	// /proc/uptime is uptime and idle time, and the idle time is larger on a
	// multi-core device: reading the wrong field reports a plausible number.
	got, ok := parseUptime("1234.56 5678.90\n")
	if !ok || got != 1234.56 {
		t.Errorf("parseUptime = %v, %v; want 1234.56, true", got, ok)
	}
}

func TestParseUptimeRejectsWhatItCannotRead(t *testing.T) {
	for _, line := range []string{"", "\n", "up 3 days", "  "} {
		if got, ok := parseUptime(line); ok {
			t.Errorf("parseUptime(%q) = %v, true; want not ok", line, got)
		}
	}
}
