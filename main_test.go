package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
)

func TestCheckName(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "a"
	}
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{"kitchen", true},
		{"echo-dot_2", true},
		{long[:63], true},
		{long, false},
		{"-kitchen", false},
		{"kitchen-", false},
		{"Kitchen", false},
		{"kitchen.local", false},
		{"kitchen dot", false},
	} {
		if err := checkName(tt.name); (err == nil) != tt.ok {
			t.Errorf("checkName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

// The port is a const here and a literal in the shell script, because shell
// cannot read a Go constant. This is what keeps them the same number: the
// script's own read-back would report a rule left behind, but not say why.
func TestUninstallDeletesTheRuleTheDaemonOpens(t *testing.T) {
	script, err := os.ReadFile("deploy/uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The whole rule, not just the port: iptables -C matches on every part of
	// it, so a moved interface leaves the delete loop finding nothing.
	want := fmt.Sprintf("-i %s -p tcp --dport %d -j ACCEPT", wifiIface, apiPort)
	if !strings.Contains(string(script), want) {
		t.Errorf("deploy/uninstall.sh deletes no rule matching %q, and the daemon adds exactly that",
			want)
	}
}

// The key path is a const here and a literal in both scripts, because shell
// cannot read a Go constant. A drift in install.sh crash-loops the daemon, which
// is loud; a drift in uninstall.sh leaves a live pre-shared key on /data while
// the script reports success, which is not.
func TestTheScriptsUseTheKeyPathTheDaemonReads(t *testing.T) {
	for _, name := range []string{"deploy/install.sh", "deploy/uninstall.sh"} {
		script, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// Two things, because either alone passes a script that has drifted: a
		// body that stopped using the variable, or an assignment typo'd by one
		// character.
		named, acts := false, false
		for _, line := range strings.Split(string(script), "\n") {
			trimmed := strings.TrimSpace(line)
			// An assignment spells the path without using it, and install.sh prints
			// it in its own advice, so neither counts as acting on it.
			inert := strings.HasPrefix(trimmed, "KEY=") ||
				strings.HasPrefix(trimmed, "#") ||
				strings.HasPrefix(trimmed, "echo ")
			if strings.Contains(line, noiseKeyPath) {
				named = true
				if !inert {
					acts = true
				}
				continue
			}
			if !inert && strings.Contains(line, "$KEY") {
				acts = true
			}
		}
		if !named {
			t.Errorf("%s never spells %q, so it acts on some other path", name, noiseKeyPath)
		}
		if !acts {
			t.Errorf("%s names %q and then never acts on it", name, noiseKeyPath)
		}
	}
}

// The one place outside the scripts that tells a user to delete the key by path.
func TestTheReadmeNamesTheKeyPathTheDaemonReads(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), noiseKeyPath) {
		t.Errorf("README.md never names %q, so its rotation instructions point elsewhere", noiseKeyPath)
	}
}

// The daemon picks the push interval and the package sets the floor, so nothing
// but this connects the two.
func TestTheSensorTickRespectsTheFloor(t *testing.T) {
	if sensorTick < esphome.MinSensorTick {
		t.Errorf("sensorTick is %v, under the %v floor PollSensors would raise it to", sensorTick, esphome.MinSensorTick)
	}
}

// PollLive has no floor of its own, and time.NewTicker panics on a tick that
// is not positive. A live tick at or above the sensor tick would also be the
// backstop it exists to beat.
func TestTheLiveTickIsPositiveAndBeatsTheSensorTick(t *testing.T) {
	if liveTick <= 0 {
		t.Errorf("liveTick is %v, and time.NewTicker panics on that", liveTick)
	}
	// The read has to finish inside the interval that made it: the poll is
	// serial, so a read outlasting it delays every reading behind it. Against
	// the heavy interval rather than the tick, because the fork happens on one
	// tick in HeavyEvery and not on the others. The budget rather than the
	// deadline, because a killed child is waited on after it.
	heavy := liveTick * esphome.HeavyEvery
	if heavy <= device.VolumeReadBudget() {
		t.Errorf("the expensive readings run every %v and one may take %v, so a slow read "+
			"delays the readings behind it", heavy, device.VolumeReadBudget())
	}
	// The split only means anything while there are ticks the heavy readings
	// skip. At one it is the old single cadence wearing a multiplier.
	if esphome.HeavyEvery < 2 {
		t.Errorf("HeavyEvery is %d, so every tick is a heavy one and the tick buys nothing",
			esphome.HeavyEvery)
	}
	// The sound read forks too, on a divisor of its own, so it needs the same
	// check against the interval that takes it.
	if soundEvery := liveTick * esphome.SoundEvery; soundEvery <= device.SpeakerReadBudget() {
		t.Errorf("sound is read every %v and one may take %v, so a slow read delays the "+
			"readings behind it", soundEvery, device.SpeakerReadBudget())
	}
	// Reading sound no oftener than the rest would be the old single cadence
	// again, and the delay it applies could not be measured any finer than
	// them.
	if esphome.SoundEvery >= esphome.HeavyEvery {
		t.Errorf("SoundEvery is %d against HeavyEvery's %d, so sound is read no oftener than "+
			"the fork it was separated from", esphome.SoundEvery, esphome.HeavyEvery)
	}
	// A delay only takes effect at a sample, so one that does not outlast the
	// interval is decided by a single reading and a single bad read moves the
	// entity. The interval here is the nominal one; PollLive is serial, so a
	// heavy tick whose fork runs long delays the next sample past it, and the
	// gap guard in readSound is what stops that lone sample deciding an edge.
	sound := liveTick * esphome.SoundEvery
	if esphome.SoundOnDelay <= sound {
		t.Errorf("SoundOnDelay is %v against a sample every %v, so one reading turns it on",
			esphome.SoundOnDelay, sound)
	}
	if esphome.SoundOffDelay <= sound {
		t.Errorf("SoundOffDelay is %v against a sample every %v, so one reading turns it off",
			esphome.SoundOffDelay, sound)
	}
	if liveTick >= sensorTick {
		t.Errorf("liveTick is %v against a sensor tick of %v, so a poll of its own buys nothing",
			liveTick, sensorTick)
	}
}

// README.md quotes this so a user can grep their log for it.
func TestTheReadmeQuotesTheMissingKeyError(t *testing.T) {
	const said = "(deploy/install.sh generates one)"
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), said) {
		t.Errorf("serve.go no longer says %q", said)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), said) {
		t.Errorf("README.md quotes an error serve.go does not write")
	}
}

// The one line that hands the button to the API, and nothing else reaches it:
// serveAPI dials no test and the switch lists either way, so deleting the call
// leaves a switch that reports the button captured for ever and moves nothing,
// with the whole suite green. Asserted over the source for the reason the port
// and the key path are: it is the only thing that can see a wiring line go.
func TestServeWiresTheButtonToTheSwitch(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "server.UseButton(") {
		t.Error("serve.go never calls UseButton, so button_capture moves nothing and says so to nobody")
	}
}

// Where a press stops being a press. The boundary belongs to the hold, so
// holding for exactly as long as the instructions say gives the hold rather
// than the press it was meant to replace.
func TestAHoldIsAPressThatOutlastsAlexasOwnThreshold(t *testing.T) {
	for _, tt := range []struct {
		held time.Duration
		want esphome.EventType
	}{
		{time.Millisecond, esphome.EventPress},
		{holdTime - time.Millisecond, esphome.EventPress},
		{holdTime, esphome.EventHold},
		{3 * time.Second, esphome.EventHold},
	} {
		if got := pressEvent(tt.held); got != tt.want {
			t.Errorf("a press held %v reported %q, want %q", tt.held, got, tt.want)
		}
	}
}

// The threshold is a promise three places make to a user in words, and nothing
// else connects them to the constant: README.md says it twice and
// docs/architecture.md once. The literal here is a further copy on purpose --
// it is what makes retuning holdTime fail rather than quietly leave the prose
// lying, and the failure names what to change with it.
func TestTheDocsPromiseTheHoldThresholdTheDaemonUses(t *testing.T) {
	const said = "six hundred milliseconds"
	const promised = 600 * time.Millisecond
	if holdTime != promised {
		t.Errorf("holdTime is %v and the docs promise %q; change them together", holdTime, said)
	}
	for _, path := range []string{"README.md", "docs/architecture.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), said) {
			t.Errorf("%s no longer says %q, which is the threshold the daemon uses", path, said)
		}
	}
}

// The other end of the same wire: serveAPI builds the server long after the
// read loop started, so a press finds it through the pointer or reaches nobody.
// Asserted over the source for the reason UseButton is -- deleting either line
// leaves a daemon that chimes, logs the press, and tells Home Assistant
// nothing, with the whole suite green.
func TestServeGivesThePressSomewhereToGo(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, said := range []string{"api.Store(server)", "server.FirePress("} {
		if !strings.Contains(string(source), said) {
			t.Errorf("serve.go never says %q, so a press reaches Home Assistant by no route", said)
		}
	}
}
