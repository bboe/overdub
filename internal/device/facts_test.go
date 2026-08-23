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

// dumpsys audio as the Dot prints it, trimmed to three streams and to the
// output devices that matter. Every stream carries a Max, and STREAM_ALARM's is
// the same 30 here, so a parser that took the first one it found would look
// correct on this device and be wrong on one where they differ. The Current
// line is the real shape: hex device masks, a name in brackets, and a level
// each, with the speaker somewhere in the middle rather than first.
const audioDump = `- STREAM_MUSIC:
   Mute count: 0
   Max: 30
   Current: 40000000 (default): 21, 2000000 (proxy): 21, 400 (hdmi): 30, 2 (speaker): 12, 4 (headset): 21, 200000 (aux_line): 30
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
		// STREAM_MUSIC is not first on every build, and every stream below it
		// has a Current line of its own that would answer just as readily.
		{"music is not the first stream",
			"- STREAM_ALARM:\n   Max: 7\n   Current: 2 (speaker): 7\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		// Leaving the block has to stop the search, or the next stream answers.
		{"music has no Current of its own",
			"- STREAM_MUSIC:\n   Max: 30\n- STREAM_ALARM:\n   Current: 2 (speaker): 7\n", 0, false},
		{"no music stream", "- STREAM_ALARM:\n   Max: 7\n   Current: 2 (speaker): 7\n", 0, false},
		{"nothing at all", "", 0, false},
		// The speaker is matched by name. A line that lists other devices and
		// not the speaker is not one this Dot can answer from.
		{"no speaker on the line",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 4 (headset): 21, 8 (headphone): 21\n", 0, false},
		{"a level that is not a number",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): none\n", 0, false},
		// No denominator, no reading. Assuming the Dot's usual 30 would report a
		// percentage on a build whose scale is not 30, and a wrong percentage
		// looks like a measurement where a missing one does not.
		{"no maximum", "- STREAM_MUSIC:\n   Current: 2 (speaker): 15\n", 0, false},
		{"a maximum of zero would divide by it",
			"- STREAM_MUSIC:\n   Max: 0\n   Current: 2 (speaker): 15\n", 0, false},
		{"a maximum that is not a number",
			"- STREAM_MUSIC:\n   Max: 15 (of 150)\n   Current: 2 (speaker): 15\n", 0, false},
		// Two maxima in one block is malformed, but it has to resolve the same way
		// every time: the first is the stream's own, printed before its levels.
		{"a second maximum inside the block does not replace the first",
			"- STREAM_MUSIC:\n   Max: 30\n   Max: 15\n   Current: 2 (speaker): 15\n", 50, true},
		{"a maximum printed after the level it scales",
			"- STREAM_MUSIC:\n   Current: 2 (speaker): 15\n   Max: 30\n", 0, false},
		{"silent", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 0\n", 0, true},
		// A muted stream keeps the level it had, so the number beside the
		// speaker is what it will return to rather than what is coming out.
		{"muted",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n   Current: 2 (speaker): 15\n", 0, true},
		{"muted more than once",
			"- STREAM_MUSIC:\n   Mute count: 3\n   Max: 30\n   Current: 2 (speaker): 15\n", 0, true},
		{"a mute count that will not parse is not a mute",
			"- STREAM_MUSIC:\n   Mute count: no\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		// The reset on a stream header, which only a dump with two blocks for
		// the same stream can show: the mute in the first must not answer for
		// the second.
		{"a mute in an earlier block of the same stream",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Max: 30\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		// Ordering inside the block is not ours to rely on: AOSP prints the
		// count first, and the answer has to be the same if it does not.
		{"a mute printed after the level",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n   Mute count: 1\n", 0, true},
		{"a mute printed after the level, in a dump that goes on",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n   Mute count: 1\n" +
				"- STREAM_ALARM:\n   Max: 30\n   Current: 2 (speaker): 7\n", 0, true},
		// First wins, as it does for Max, rather than whichever came last.
		{"two mute counts",
			"- STREAM_MUSIC:\n   Mute count: 1\n   Mute count: 0\n   Max: 30\n" +
				"   Current: 2 (speaker): 15\n", 0, true},
		// The known-wrong shape, and the reason README and docs both say so: a
		// cable in the line out is the level somebody hears, and this is not it.
		{"a level on the aux jack is not the speaker's",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 200000 (aux_line): 30, 2 (speaker): 12\n", 40, true},
		{"a mute on another stream",
			"- STREAM_ALARM:\n   Mute count: 1\n   Max: 30\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n", 50, true},
		// The speaker first on the line, which this Dot never prints and another
		// build might: the label has to be stripped before the fields are read.
		{"the speaker listed first",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15, 4 (headset): 21\n", 50, true},
		{"full", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 30\n", 100, true},
		// The two numbers come from one call now, so they cannot disagree by
		// timing -- but a level above the scale is still not a percentage.
		{"above the maximum", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 44\n", 100, true},
		{"below zero", "- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): -1\n", 0, true},
		// The block ends at the left margin, not at the end of the dump. Without
		// that, a section printed after the last stream answers for the stream,
		// and it answers with a number that looks like a volume.
		{"a later section carries Max and Current of its own",
			"- STREAM_MUSIC:\n   Mute count: 0\n   Max: 30\n\n" +
				"Ringer mode:\n   Max: 7\n   Current: 2 (speaker): 7\n", 0, false},
		{"music is the last stream and reads normally",
			"- STREAM_ALARM:\n   Max: 7\n   Current: 2 (speaker): 7\n" +
				"- STREAM_MUSIC:\n   Max: 30\n   Current: 2 (speaker): 15\n\n" +
				"Ringer mode:\n   Max: 7\n", 50, true},
		// speaker_safe is an output device of its own with a level of its own, so
		// the name has to match whole rather than be contained.
		{"a near-miss device name listed before the speaker",
			"- STREAM_MUSIC:\n   Max: 30\n" +
				"   Current: 40000000 (default): 21, 1000000 (speaker_safe): 3, 2 (speaker): 12\n", 40, true},
		{"only a near-miss device name",
			"- STREAM_MUSIC:\n   Max: 30\n   Current: 1000000 (speaker_safe): 3\n", 0, false},
		// Real dumps carry more than the two lines this parser wants, and print
		// a header before the streams.
		{"the lines a real dump prints around the ones we read",
			"Stream volumes (device: index)\n- STREAM_MUSIC:\n   Mute count: 0\n   Min: 0\n" +
				"   Max: 30\n   streamVolume:12\n   Current: 2 (speaker): 12\n   Devices: speaker\n", 40, true},
	}
	for _, tt := range tests {
		got, ok := parseMusicVolume(tt.dump)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: parseMusicVolume = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

// The volume read forks a process and talks to binder, and binder can wedge. A
// read that never answers has to cost one reading rather than every reading
// after it, because nothing else would ever notice the poll had stopped.
func TestTheVolumeReadGivesUpRatherThanHanging(t *testing.T) {
	wasCmd, wasWait := volumeCommand, volumeReadTimeout
	defer func() { volumeCommand, volumeReadTimeout = wasCmd, wasWait }()

	volumeReadTimeout = 100 * time.Millisecond
	started := make(chan struct{})
	volumeCommand = func(ctx context.Context) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan struct{})
	var got float32
	var ok bool
	go func() { got, ok = MusicVolumePercent(); close(done) }()

	<-started
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never gave up, so a wedged binder stops the volume for the rest of the boot")
	}
	if ok {
		t.Errorf("a read that never answered reported %v as a measurement", got)
	}
}

// The bound is two numbers and this is the one no stub can show. Killing the
// child does not end the call: Output waits for the pipe to reach EOF, and a
// child that forked one of its own leaves that pipe open behind it. The shell
// below is exactly that shape, so a run without WaitDelay waits out the
// grandchild rather than the deadline, and one that defines the budget as the
// deadline alone runs over the number main_test.go holds against the tick.
func TestOneReadCannotOutlastItsBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	wasArgv, wasTimeout, wasDelay := volumeArgv, volumeReadTimeout, volumeWaitDelay
	defer func() {
		volumeArgv, volumeReadTimeout, volumeWaitDelay = wasArgv, wasTimeout, wasDelay
	}()

	// Scaled down so the test costs half a second rather than two, and weighted
	// towards the wait: a budget that left it out would be a quarter of this
	// one, which the assertion below can tell from the whole.
	volumeReadTimeout, volumeWaitDelay = 100*time.Millisecond, 400*time.Millisecond
	// The grandchild outlives the kill and holds the inherited stdout open.
	volumeArgv = []string{"sh", "-c", "sleep 30 & sleep 30"}

	start := time.Now()
	got, ok := MusicVolumePercent()
	elapsed := time.Since(start)

	if ok {
		t.Errorf("a read that never answered reported %v", got)
	}
	// A lower bound as well as an upper one. Without it the test passes when the
	// command never ran at all -- a wrong argv fails instantly on any machine
	// with no /system/bin, and an instant failure satisfies every assertion
	// below about a call that finishes inside its budget.
	if elapsed < volumeReadTimeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	// Slack enough for a loaded machine and not enough to hide the deadline
	// standing in for the whole budget: a correct read lands on the budget, and
	// one measured against the deadline alone is four times over it.
	if limit := VolumeReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, VolumeReadBudget())
	}
}

// Millidegrees, and the two bounds that separate a reading from a zone with
// nothing to say. Measured on the Dot: mtktscpu answered 41300.
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
		// The kernel's own "nothing to report" answer, and the reason the lower
		// bound is not a test for zero.
		{"the invalid marker", "-127000\n", 0, false},
		{"below the bound", "-40000\n", 0, false},
		{"above the bound", "150000\n", 0, false},
		{"not a number", "warm\n", 0, false},
		{"empty", "", 0, false},
		// Zero millidegrees is 0C, which no powered SoC indoors reports, but it
		// is inside the bounds and is passed through rather than guessed at.
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

// The zone is found by its type, because the index is not stable and the names
// do not even sort into their own order: thermal_zone10 comes before
// thermal_zone2. The layout below is this Dot's, with the CPU zone deliberately
// neither first nor last.
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
		// A different SoC has no zone of this type, and a reading nobody can
		// take is missing rather than another zone's answer.
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
			// A stray entry that is not a zone at all: the real directory holds
			// fifty-four cooling_device* beside the zones.
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

// Nothing to read at all, which is every machine this suite runs on.
func TestCPUTemperatureWithNoThermalDirectory(t *testing.T) {
	was := thermalRoot
	defer func() { thermalRoot, cpuZonePath = was, "" }()
	thermalRoot, cpuZonePath = filepath.Join(t.TempDir(), "absent"), ""

	if got, ok := CPUTemperature(); ok {
		t.Errorf("CPUTemperature() reported %v with no thermal directory", got)
	}
}

// The search is skipped after the first reading, and picked up again if the
// path it found stops working.
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

	// The type file is what the search reads. Removing it leaves the remembered
	// path working, so a second reading that still succeeds did not search.
	if err := os.Remove(filepath.Join(zone, "type")); err != nil {
		t.Fatal(err)
	}
	write("temp", "42000")
	if got, ok := CPUTemperature(); got != 42 || !ok {
		t.Errorf("second reading = %v, %v; want 42, true -- it searched again", got, ok)
	}

	// And when the path itself stops working, the next reading looks again
	// rather than reporting nothing for the rest of the boot.
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
	// The Dot as it stands, trimmed to the lines around the one that is read.
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
		// MemFree is the number that looks alarming and is not this one. The
		// match carries its colon, so this is the guard against a parser keyed
		// on something shorter rather than against a prefix of the full name.
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
