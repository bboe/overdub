package device

import "testing"

func TestRebootAsksInitThroughPowerctl(t *testing.T) {
	oldSet := setProp
	defer func() { setProp = oldSet }()
	var key, value string
	setProp = func(k, v string) error { key, value = k, v; return nil }
	if err := Reboot(); err != nil {
		t.Fatal(err)
	}
	if key != "sys.powerctl" || value != "reboot" {
		t.Errorf("set %s=%s, want sys.powerctl=reboot", key, value)
	}
}
