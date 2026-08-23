package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

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

// The daemon picks the push interval and the package enforces the deadline, so
// nothing but this connects the two.
func TestTheSensorTickDoesNotSilenceTheClient(t *testing.T) {
	if sensorTick < esphome.MinSensorTick {
		t.Errorf("sensorTick is %v, under the %v that keeps a connection alive", sensorTick, esphome.MinSensorTick)
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
