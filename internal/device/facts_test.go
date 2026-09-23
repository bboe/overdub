package device

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParseUptimeTakesTheFirstField(t *testing.T) {
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
		{"the row a reader a minute apart sees",
			" wlan0: 0000    0.  208.    0.       0      0      0      0      0        0\n",
			"wlan0", -48, true},
		{"the weakest signal still believed", " wlan0: 0000    0   137     0\n", "wlan0", -119, true},
		{"one step below the noise floor", " wlan0: 0000    0   136     0\n", "wlan0", 0, false},
		{"an unwrapped value is not a signal", " wlan0: 0000    0   127     0\n", "wlan0", 0, false},
		{"a Dot beside its access point", " wlan0: 0000    0   231     0\n", "wlan0", -25, true},
		{"the strongest signal still believed", " wlan0: 0000    0   255     0\n", "wlan0", -1, true},
		{"an interface with no statistics", table, "p2p0", 0, false},
		{"an interface that is not there", table, "wlan9", 0, false},
		{"a longer name is not ours", table, "lan0", 0, false},
		{"a longer interface is not ours",
			" wlan01: 0000    0   231     0\n", "wlan0", 0, false},
		{"a line with too few columns", " wlan0: 0000    0\n", "wlan0", 0, false},
		{"a level that is not a number", " wlan0: 0000    0   n/a     0\n", "wlan0", 0, false},
		{"nothing at all", "", "wlan0", 0, false},
		{"a level that is already negative", " wlan0: 0000    0   -48     0\n", "wlan0", -48, true},
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

const audioDump = `- STREAM_MUSIC:
   Mute count: 0
   Max: 30
   Current: 40000000 (default): 21, 2000000 (proxy): 21, 400 (hdmi): 30, 80 (bt_a2dp): 24, 100 (bt_a2dp_hp): 18, 200 (bt_a2dp_spk): 9, 2 (speaker): 12, 4 (headset): 21, 200000 (aux_line): 30
- STREAM_ALARM:
   Mute count: 0
   Max: 30
   Current: 40000000 (default): 21, 2 (speaker): 21
- STREAM_NOTIFICATION:
   Mute count: 0
   Max: 15
   Current: 40000000 (default): 9, 2 (speaker): 9
`

func TestParseMusicVolume(t *testing.T) {
	tests := []struct {
		name string
		dump string
		want float32
		ok   bool
	}{
		{"the Dot as it stands", audioDump, 40, true},
		{"music is not the first stream",
			"- STREAM_ALARM:\n   Max: 7\n   Current: 2 (speaker): 7\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		{"music has no Current of its own",
			"- STREAM_MUSIC:\n   Max: 30\n- STREAM_ALARM:\n   Current: 2 (speaker): 7\n", 0, false},
		{"no music stream", "- STREAM_ALARM:\n   Max: 7\n   Current: 2 (speaker): 7\n", 0, false},
		{"nothing at all", "", 0, false},
		{"no speaker on the line",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 4 (headset): 21, 8 (headphone): 21\n", 0, false},
		{"a level that is not a number",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): none\n", 0, false},
		{"no maximum", "- STREAM_MUSIC:\n   Current: 2 (speaker): 15\n", 0, false},
		{"a maximum of zero would divide by it",
			"- STREAM_MUSIC:\n   Max: 0\n   Current: 2 (speaker): 15\n", 0, false},
		{"a maximum that is not a number",
			"- STREAM_MUSIC:\n   Max: 15 (of 150)\n   Current: 2 (speaker): 15\n", 0, false},
		{"a second maximum inside the block does not replace the first",
			"- STREAM_MUSIC:\n   Max: 30\n   Max: 15\n   Current: 2 (speaker): 15\n", 50, true},
		{"a maximum printed after the level it scales",
			"- STREAM_MUSIC:\n   Current: 2 (speaker): 15\n   Max: 30\n", 0, false},
		{"silent", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 0\n", 0, true},
		{"muted",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n   Current: 2 (speaker): 15\n", 0, true},
		{"muted more than once",
			"- STREAM_MUSIC:\n   Mute count: 3\n   Max: 30\n   Current: 2 (speaker): 15\n", 0, true},
		{"a mute count that will not parse is not a mute",
			"- STREAM_MUSIC:\n   Mute count: no\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		{"a mute in an earlier block of the same stream",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		{"a mute printed after the level",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n   Mute count: 1\n", 0, true},
		{"a mute printed after the level, in a dump that goes on",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n   Mute count: 1\n" +
				"- STREAM_ALARM:\n   Max: 30\n   Current: 2 (speaker): 7\n", 0, true},
		{"two mute counts",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Mute count: 0\n   Max: 30\n" +
				"   Current: 2 (speaker): 15\n", 0, true},
		{"a level on the aux jack is not the speaker's",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 200000 (aux_line): 30, 2 (speaker): 12\n", 40, true},
		{"a mute on another stream",
			"- STREAM_ALARM:\n   Mute count: 1\n   Max: 30\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		{"the speaker listed first",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15, 4 (headset): 21\n", 50, true},
		{"full", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 30\n", 100, true},
		{"above the maximum", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 44\n", 100, true},
		{"below zero", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): -1\n", 0, true},
		{"a later section carries Max and Current of its own",
			"- STREAM_MUSIC:\n   Mute count: 0\n   Max: 30\n\n" +
				"Ringer mode:\n   Max: 7\n   Current: 2 (speaker): 7\n", 0, false},
		{"music is the last stream and reads normally",
			"- STREAM_ALARM:\n   Max: 7\n   Current: 2 (speaker): 7\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n\n" +
				"Ringer mode:\n   Max: 7\n", 50, true},
		{"a near-miss device name listed before the speaker",
			"- STREAM_MUSIC:\n   Max: 30\n" +
				"   Current: 40000000 (default): 21, 1000000 (speaker_safe): 3, 2 (speaker): 12\n", 40, true},
		{"only a near-miss device name",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 1000000 (speaker_safe): 3\n", 0, false},
		{"the lines a real dump prints around the ones we read",
			"Stream volumes (device: index)\n- STREAM_MUSIC:\n   Mute count: 0\n   Min: 0\n" +
				"   Max: 30\n   streamVolume:12\n   Current: 2 (speaker): 12\n   Devices: speaker\n", 40, true},
	}
	for _, tt := range tests {
		v := parseMusicVolumes(tt.dump)
		if v.Speaker != tt.want || v.SpeakerOK != tt.ok {
			t.Errorf("%s: speaker = %v, %v; want %v, %v", tt.name, v.Speaker, v.SpeakerOK, tt.want, tt.ok)
		}
	}
}

func TestParseJackVolume(t *testing.T) {
	for _, tt := range []struct {
		name string
		dump string
		want float32
		ok   bool
	}{
		{"the Dot as it stands", audioDump, 70, true},
		{"headphone when there is no headset",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 12, 8 (headphone): 15\n", 50, true},
		{"headset wins when both are listed",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 4 (headset): 9, 8 (headphone): 30\n", 30, true},
		{"neither is listed",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 12\n", 0, false},
		{"muted",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n   Current: 4 (headset): 9\n", 0, true},
		{"no maximum, no reading",
			"- STREAM_MUSIC:\n   Current: 4 (headset): 9\n", 0, false},
		{"a longer name ending in ours is not ours",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 12, 4000000 (usb_headset): 30\n", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := parseMusicVolumes(tt.dump)
			if v.Jack != tt.want || v.JackOK != tt.ok {
				t.Errorf("jack = %v, %v; want %v, %v", v.Jack, v.JackOK, tt.want, tt.ok)
			}
		})
	}
}

func TestParseBluetoothVolume(t *testing.T) {
	for _, tt := range []struct {
		name string
		dump string
		want float32
		ok   bool
	}{
		{"the Dot with a speaker paired", audioDump, 80, true},
		{"the generic route wins when all three are listed",
			"- STREAM_MUSIC:\n   Max: 30\n" +
				"   Current: 80 (bt_a2dp): 9, 100 (bt_a2dp_hp): 30, 200 (bt_a2dp_spk): 30\n", 30, true},
		{"headphones when the generic route is absent",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 100 (bt_a2dp_hp): 15\n", 50, true},
		{"a speaker when the generic route is absent",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 200 (bt_a2dp_spk): 15\n", 50, true},
		{"headphones win over a speaker when the generic route is absent",
			"- STREAM_MUSIC:\n   Max: 30\n" +
				"   Current: 100 (bt_a2dp_hp): 9, 200 (bt_a2dp_spk): 30\n", 30, true},
		{"sco is not a2dp",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 10 (bt_sco): 21, 2 (speaker): 12\n", 0, false},
		{"nothing is paired",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 12\n", 0, false},
		{"muted",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n   Current: 80 (bt_a2dp): 9\n", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := parseMusicVolumes(tt.dump)
			if v.Bluetooth != tt.want || v.BluetoothOK != tt.ok {
				t.Errorf("bluetooth = %v, %v; want %v, %v", v.Bluetooth, v.BluetoothOK, tt.want, tt.ok)
			}
		})
	}
}

func TestABluetoothLevelIsAReadingOnItsOwn(t *testing.T) {
	v := parseMusicVolumes("- STREAM_MUSIC:\n   Max: 30\n   Current: 80 (bt_a2dp): 9\n")
	if !v.BluetoothOK || v.BluetoothStep != 9 || v.Max != 30 {
		t.Errorf("a dump naming no speaker and no jack gave %+v, want the bluetooth step alone", v)
	}
}

func TestParseMusicVolumeSteps(t *testing.T) {
	for _, tt := range []struct {
		name      string
		dump      string
		max       int
		speaker   int
		speakerOK bool
		jack      int
		jackOK    bool
		muted     bool
	}{
		{"the Dot as it stands", audioDump, 30, 12, true, 21, true, false},
		{"above the maximum clamps to it",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 44, 4 (headset): 31\n",
			30, 30, true, 30, true, false},
		{"below zero clamps to it, rather than reading as no level at all",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): -1, 4 (headset): -9\n",
			30, 0, true, 0, true, false},
		{"a muted stream still names the step it will return to",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n" +
				"   Current: 2 (speaker): 15, 4 (headset): 9\n", 30, 15, true, 9, true, true},
		{"no maximum, no step and no scale",
			"- STREAM_MUSIC:\n   Current: 2 (speaker): 15\n", 0, 0, false, 0, false, false},
		{"a route that is not listed has no step",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 30, 15, true, 0, false, false},
		{"the headphone step when there is no headset",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 12, 8 (headphone): 15\n",
			30, 12, true, 15, true, false},
		{"a scale with no route to apply it to is not a reading",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 1000000 (speaker_safe): 3\n",
			0, 0, false, 0, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := parseMusicVolumes(tt.dump)
			if v.Max != tt.max || v.SpeakerStep != tt.speaker || v.JackStep != tt.jack {
				t.Errorf("max = %d, speaker = %d, jack = %d; want %d, %d, %d",
					v.Max, v.SpeakerStep, v.JackStep, tt.max, tt.speaker, tt.jack)
			}
			if v.SpeakerOK != tt.speakerOK || v.JackOK != tt.jackOK {
				t.Errorf("read speaker = %v, jack = %v; want %v, %v",
					v.SpeakerOK, v.JackOK, tt.speakerOK, tt.jackOK)
			}
			if v.Muted != tt.muted {
				t.Errorf("muted = %v, want %v: the mute is a fact of its own, and a caller "+
					"reading it back out of a zeroed percentage cannot tell it from a level "+
					"somebody turned down", v.Muted, tt.muted)
			}
		})
	}
}

func TestParseJackSwitch(t *testing.T) {
	for _, tt := range []struct {
		name     string
		state    string
		occupied bool
		ok       bool
	}{
		{"nothing in the jack", "0\n", false, true},
		{"something in the jack", "1\n", true, true},
		{"a plug it thinks has no microphone", "2\n", true, true},
		{"no newline", "1", true, true},
		{"a state we do not know", "3\n", false, false},
		{"not a number", "yes\n", false, false},
		{"empty", "", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state")
			if err := os.WriteFile(path, []byte(tt.state), 0o644); err != nil {
				t.Fatal(err)
			}
			was := jackSwitchPath
			defer func() { jackSwitchPath = was }()
			jackSwitchPath = path

			occupied, ok := JackOccupied()
			if occupied != tt.occupied || ok != tt.ok {
				t.Errorf("JackOccupied() = %v, %v; want %v, %v", occupied, ok, tt.occupied, tt.ok)
			}
		})
	}
}

func TestJackOccupiedWithNoSwitch(t *testing.T) {
	was := jackSwitchPath
	defer func() { jackSwitchPath = was }()
	jackSwitchPath = filepath.Join(t.TempDir(), "absent")

	if _, ok := JackOccupied(); ok {
		t.Error("JackOccupied() reported a state with no switch to read")
	}
}

func TestTheVolumeReadGivesUpRatherThanHanging(t *testing.T) {
	defer func(was dumpsys) { *volumeDump = was }(*volumeDump)

	volumeDump.timeout = 100 * time.Millisecond
	started := make(chan struct{})
	volumeDump.command = func(ctx context.Context) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan struct{})
	var got MusicVolume
	go func() { got = MusicVolumes(); close(done) }()

	<-started
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never gave up, so a wedged binder stops the volume for the rest of the boot")
	}
	if got.SpeakerOK || got.JackOK {
		t.Errorf("a read that never answered reported %+v as a measurement", got)
	}
}

func TestOneReadCannotOutlastItsBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	defer func(was dumpsys) { *volumeDump = was }(*volumeDump)

	volumeDump.timeout, volumeDump.wait = 100*time.Millisecond, 400*time.Millisecond
	volumeDump.argv = []string{"sh", "-c", "sleep 30 & sleep 30"}

	start := time.Now()
	got := MusicVolumes()
	elapsed := time.Since(start)

	if got.SpeakerOK || got.JackOK {
		t.Errorf("a read that never answered reported %+v", got)
	}
	if elapsed < volumeDump.timeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	if limit := VolumeReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, VolumeReadBudget())
	}
}

func TestParseMilliCelsius(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want float32
		ok   bool
	}{
		{"the Dot as it stands", "41300\n", 41.3, true},
		{"no newline", "41300", 41.3, true},
		{"carriage return too", "41300\r\n", 41.3, true},
		{"a tenth is kept", "41305\n", 41.305, true},
		{"cool but plausible", "-39000\n", -39, true},
		{"hot but plausible", "149000\n", 149, true},
		{"the invalid marker", "-127000\n", 0, false},
		{"below the bound", "-40000\n", 0, false},
		{"above the bound", "150000\n", 0, false},
		{"not a number", "warm\n", 0, false},
		{"empty", "", 0, false},
		{"zero", "0\n", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseMilliCelsius(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseMilliCelsius(%q) = %v, %v; want %v, %v", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCPUTemperatureFindsItsZoneByType(t *testing.T) {
	zones := map[string]string{
		"thermal_zone0":  "mtktswmt",
		"thermal_zone1":  "mtktscpu",
		"thermal_zone10": "mtkts_bts2",
		"thermal_zone2":  "mtkts1",
		"thermal_zone7":  "tmp103",
	}
	temps := map[string]string{
		"thermal_zone0":  "37000",
		"thermal_zone1":  "41300",
		"thermal_zone10": "37000",
		"thermal_zone2":  "39400",
		"thermal_zone7":  "35606",
	}

	for _, tt := range []struct {
		name string
		drop string
		want float32
		ok   bool
	}{
		{"the CPU zone is found among the others", "", 41.3, true},
		{"no zone of that type", "thermal_zone1", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for zone, kind := range zones {
				if zone == tt.drop {
					continue
				}
				dir := filepath.Join(root, zone)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				write := func(name, body string) {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				write("type", kind)
				write("temp", temps[zone])
			}
			if err := os.MkdirAll(filepath.Join(root, "cooling_device0"), 0o755); err != nil {
				t.Fatal(err)
			}

			was := thermalRoot
			defer func() { thermalRoot, cpuZonePath = was, "" }()
			thermalRoot, cpuZonePath = root, ""

			got, ok := CPUTemperature()
			if got != tt.want || ok != tt.ok {
				t.Errorf("CPUTemperature() = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCPUTemperatureWithNoThermalDirectory(t *testing.T) {
	was := thermalRoot
	defer func() { thermalRoot, cpuZonePath = was, "" }()
	thermalRoot, cpuZonePath = filepath.Join(t.TempDir(), "absent"), ""

	if got, ok := CPUTemperature(); ok {
		t.Errorf("CPUTemperature() reported %v with no thermal directory", got)
	}
}

func TestTheThermalZoneIsFoundOnceAndThenRead(t *testing.T) {
	root := t.TempDir()
	zone := filepath.Join(root, "thermal_zone1")
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(zone, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("type", "mtktscpu")
	write("temp", "41300")

	was := thermalRoot
	defer func() { thermalRoot, cpuZonePath = was, "" }()
	thermalRoot, cpuZonePath = root, ""

	if got, ok := CPUTemperature(); got != 41.3 || !ok {
		t.Fatalf("first reading = %v, %v; want 41.3, true", got, ok)
	}
	if cpuZonePath == "" {
		t.Error("the zone was not remembered, so every reading pays for the search")
	}

	if err := os.Remove(filepath.Join(zone, "type")); err != nil {
		t.Fatal(err)
	}
	write("temp", "42000")
	if got, ok := CPUTemperature(); got != 42 || !ok {
		t.Errorf("second reading = %v, %v; want 42, true -- it searched again", got, ok)
	}

	if err := os.Remove(filepath.Join(zone, "temp")); err != nil {
		t.Fatal(err)
	}
	if _, ok := CPUTemperature(); ok {
		t.Error("a zone whose temp file is gone still reported a reading")
	}
	if cpuZonePath != "" {
		t.Error("the dead path was kept, so nothing will ever look for the zone again")
	}
	write("type", "mtktscpu")
	write("temp", "39000")
	if got, ok := CPUTemperature(); got != 39 || !ok {
		t.Errorf("after the zone came back, reading = %v, %v; want 39, true", got, ok)
	}
}

func TestParseAvailableMemory(t *testing.T) {
	const meminfo = `MemTotal:         482956 kB
MemFree:           36344 kB
MemAvailable:     129196 kB
Buffers:            8420 kB
Cached:           126472 kB
`
	for _, tt := range []struct {
		name string
		info string
		want float32
		ok   bool
	}{
		{"the Dot as it stands", meminfo, 126.16797, true},
		{"first line", "MemAvailable:     129196 kB\n", 126.16797, true},
		{"no trailing newline", "MemAvailable:     129196 kB", 126.16797, true},
		{"zero is a reading", "MemAvailable:          0 kB\n", 0, true},
		{"MemFree is not MemAvailable", "MemFree:           36344 kB\n", 0, false},
		{"absent before Linux 3.14", "MemTotal:         482956 kB\nMemFree: 36344 kB\n", 0, false},
		{"a unit we did not expect", "MemAvailable:     129196 MB\n", 0, false},
		{"no unit at all", "MemAvailable:     129196\n", 0, false},
		{"not a number", "MemAvailable:       lots kB\n", 0, false},
		{"negative", "MemAvailable:      -12 kB\n", 0, false},
		{"empty", "", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseAvailableMemory(tt.info)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseAvailableMemory() = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestHasIPv4RefusesLoopbackAndInterfacesThatAreNotThere(t *testing.T) {
	if HasIPv4("lo") {
		t.Error("loopback counts as an address, so the advert would go up before wlan0" +
			" had one a server can reach")
	}
	if HasIPv4("overdub-no-such-interface") {
		t.Error("an interface that does not exist reads as addressed")
	}
}

func TestWaitForIPv4StopsAtItsDeadlineAndOnItsChannel(t *testing.T) {
	start := time.Now()
	if WaitForIPv4("overdub-no-such-interface", 50*time.Millisecond, nil) {
		t.Error("reported an address on an interface that does not exist")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("waited %v for a 50ms limit, so the deadline does not bound it", took)
	}

	stop := make(chan struct{})
	close(stop)
	start = time.Now()
	if WaitForIPv4("overdub-no-such-interface", time.Hour, stop) {
		t.Error("reported an address after being told to stop")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("waited %v with the stop channel already closed, so a teardown would"+
			" sit behind the whole address wait", took)
	}
}

func TestAMuteZeroesThePercentageAndNotTheStep(t *testing.T) {
	v := parseMusicVolumes("- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n" +
		"   Current: 2 (speaker): 15, 4 (headset): 9\n")
	if !v.Muted {
		t.Fatal("a stream with a mute count did not read as muted")
	}
	if v.Speaker != 0 || v.Jack != 0 {
		t.Errorf("a muted stream reported %v%% and %v%%, want zero on both: zero is what"+
			" can be heard", v.Speaker, v.Jack)
	}
	if v.SpeakerStep != 15 || v.JackStep != 9 {
		t.Errorf("a muted stream reported steps %d and %d, want 15 and 9: the level a"+
			" mute is holding is where a change counts from, so zeroing it makes every"+
			" volume set while muted land at the bottom", v.SpeakerStep, v.JackStep)
	}
}

const routesDump = `Audio routes:
  mMainType=0x0
  mBluetoothName=Device Connected

Other state:
  mVolumeController=VolumeController(android.os.BinderProxy@3a1fcea0,mVisible=false)
`

func TestParseBluetoothRoute(t *testing.T) {
	for _, tt := range []struct {
		name      string
		dump      string
		connected bool
		ok        bool
	}{
		{"a speaker is connected", routesDump, true, true},
		{"nothing is connected",
			"Audio routes:\n  mMainType=0x0\n  mBluetoothName=Device NOT Connected\n", false, true},
		{"a build that names the device instead",
			"Audio routes:\n  mMainType=0x0\n  mBluetoothName=JBL Go 3\n", false, false},
		{"no routes block at all",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 12\n", false, false},
		{"the block carries no bluetooth line",
			"Audio routes:\n  mMainType=0x0\n\nOther state:\n  mMcc=0\n", false, false},
		{"a later block carries one instead",
			"Audio routes:\n  mMainType=0x0\nOther state:\n  mBluetoothName=Device Connected\n",
			false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			connected, ok := parseBluetoothRoute(tt.dump)
			if connected != tt.connected || ok != tt.ok {
				t.Errorf("route = %v, %v; want %v, %v", connected, ok, tt.connected, tt.ok)
			}
		})
	}
}
