package device

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestInputRule(t *testing.T) {
	got := inputRule(6053)
	want := []string{"INPUT", "-i", "wlan0", "-p", "tcp", "--dport", "6053", "-j", "ACCEPT"}
	if len(got) != len(want) {
		t.Fatalf("inputRule = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInputRuleUsesThePortGiven(t *testing.T) {
	got := strings.Join(inputRule(5555), " ")
	if !strings.Contains(got, "--dport 5555") {
		t.Errorf("inputRule(5555) = %q, want it to carry --dport 5555", got)
	}
}

func exitWith(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("a shell exiting %d reported success", code)
	}
	return err
}

func TestPresentSeparatesAbsentFromBroken(t *testing.T) {
	saved := iptablesRun
	defer func() { iptablesRun = saved }()

	for _, tt := range []struct {
		name    string
		run     func(...string) ([]byte, error)
		want    bool
		wantErr bool
	}{
		{"rule is there", func(...string) ([]byte, error) { return nil, nil }, true, false},
		// iptables exits 1 for "no such rule". Anything else means the check itself
		// failed, and treating that as "absent" appends a duplicate every time. 2 is
		// "bad argument", which is what an iptables too old for -C gives.
		{"rule is absent", func(...string) ([]byte, error) { return nil, exitWith(t, 1) }, false, false},
		{"check is broken", func(...string) ([]byte, error) { return nil, exitWith(t, 2) }, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			iptablesRun = tt.run
			got, err := present(inputRule(6053))
			if got != tt.want {
				t.Errorf("present = %v, want %v", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("present error = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestAllowTCPAppendsOnlyWhenTheRuleIsMissing(t *testing.T) {
	saved := iptablesRun
	defer func() { iptablesRun = saved }()

	var appends int
	present := false
	iptablesRun = func(args ...string) ([]byte, error) {
		switch args[0] {
		case "-C":
			if present {
				return nil, nil
			}
			return nil, exitWith(t, 1)
		case "-A":
			appends++
			present = true
		}
		return nil, nil
	}
	for i := 0; i < 5; i++ {
		if err := AllowTCP(6053); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if appends != 1 {
		t.Errorf("appended the rule %d times over 5 calls, want 1: the INPUT chain "+
			"grows a duplicate on every re-assert", appends)
	}
}
