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

// /proc/net/wireless copied off the Dot. The level is an unsigned byte, so -48
// dBm arrives as 208, and p2p0 is here with every column zero because it is a
// wireless netdev with no statistics to report. This is the form a second read
// gets; the dotted form below is what a reader a minute apart sees, and that is
// this daemon.
func TestParseWifiLevel(t *testing.T) {
	const table = `Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE
 face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22
 wlan0: 0000    0   208     0        0      0      0      0      0        0
  p2p0: 0000    0     0     0        0      0      0      0      0        0
`
	tests := []struct {
		name  string
		table string
		iface string
		want  float32
		ok    bool
	}{
		{"wraps to a negative dBm", table, "wlan0", -48, true},
		// Reading the file clears the updated flags, so the dots are there for the
		// first read after the driver refreshes its statistics and gone for every
		// read until the next one. Measured on the Dot: one dotted row, then nine
		// plain ones in the same burst. A sensor polled once a minute gets the
		// dotted form every time.
		{"the row a reader a minute apart sees",
			" wlan0: 0000    0.  208.    0.       0      0      0      0      0        0\n",
			"wlan0", -48, true},
		// 137 and 136 sit either side of the weakest signal worth believing, and
		// they only land there once 256 has come off: without the wrap they read
		// as positive, and every reading this sensor takes would be refused.
		{"the weakest signal still believed", " wlan0: 0000    0   137     0\n", "wlan0", -119, true},
		{"one step below the noise floor", " wlan0: 0000    0   136     0\n", "wlan0", 0, false},
		{"an unwrapped value is not a signal", " wlan0: 0000    0   127     0\n", "wlan0", 0, false},
		// The other edge. A Dot in the same room as its access point reads
		// around -25, so a window that stopped short of it would leave that
		// device reporting nothing at all, with no log line to say why.
		{"a Dot beside its access point", " wlan0: 0000    0   231     0\n", "wlan0", -25, true},
		{"the strongest signal still believed", " wlan0: 0000    0   255     0\n", "wlan0", -1, true},
		// Zero is what the kernel prints for a wireless interface it has no
		// statistics for, which wlan0 itself is until it associates. Reported as
		// a reading it would be 0 dBm, the strongest signal Home Assistant can
		// draw, rather than the gap it is.
		{"an interface with no statistics", table, "p2p0", 0, false},
		{"an interface that is not there", table, "wlan9", 0, false},
		// The name has to match the whole field, colon included: a prefix match
		// without it takes wlan01's row for wlan0's, and the two are different
		// radios.
		{"a longer name is not ours", table, "lan0", 0, false},
		{"a longer interface is not ours",
			" wlan01: 0000    0   231     0\n", "wlan0", 0, false},
		{"a line with too few columns", " wlan0: 0000    0\n", "wlan0", 0, false},
		{"a level that is not a number", " wlan0: 0000    0   n/a     0\n", "wlan0", 0, false},
		{"nothing at all", "", "wlan0", 0, false},
		// Some drivers hand the kernel a level that is already signed, and it
		// arrives here without wrapping.
		{"a level that is already negative", " wlan0: 0000    0   -48     0\n", "wlan0", -48, true},
		// struct iw_quality.level is a __u8, so nothing negative can come from a
		// driver. The kernel writes one when the driver sets IW_QUAL_DBM: it
		// takes 0x100 off itself, which makes that driver's no-reading value
		// -256 rather than zero. Neither is a signal, and neither is anything at
		// or above 0 dBm.
		{"the dBm driver's empty reading", " wlan0: 0000    0  -256     0\n", "wlan0", 0, false},
		{"a positive level is not a signal", " wlan0: 0000    0    12     0\n", "wlan0", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseWifiLevel(tt.table, tt.iface)
		if ok != tt.ok || got != tt.want {
			t.Errorf("%s: parseWifiLevel = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}
