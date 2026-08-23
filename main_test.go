package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
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
