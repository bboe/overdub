package device

import (
	"slices"
	"testing"
)

func TestEveryDumpsysReadAsksItsServiceAndIsBounded(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dump    *dumpsys
		service string
	}{
		{"volumes", volumeDump, "audio"},
		{"registration", accountDump, "account"},
		{"bluetooth device", btDump, "bluetooth_manager"},
		{"speaker", speakerDump, "media.audio_flinger"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if want := []string{"/system/bin/dumpsys", tt.service}; !slices.Equal(tt.dump.argv, want) {
				t.Errorf("the read runs %v, want %v", tt.dump.argv, want)
			}
			if tt.dump.timeout <= 0 {
				t.Errorf("the deadline is %v, so a wedged binder holds the poll forever", tt.dump.timeout)
			}
			if tt.dump.wait <= 0 {
				t.Errorf("the wait delay is %v, so a grandchild holding the pipe holds the poll "+
					"forever after the deadline", tt.dump.wait)
			}
			if tt.dump.command != nil {
				t.Error("a test's stand-in command is still installed")
			}
		})
	}
}
