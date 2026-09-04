package device

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:15B3 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 23456 1 0000000000000000 100 0 0 10 0
`

func TestHasListenerFindsADB(t *testing.T) {
	if !hasListener(procNetTCP, adbPortHex) {
		t.Error("hasListener did not find adbd listening on 5555")
	}
}

func TestHasListenerRejects(t *testing.T) {
	established := `   0: 00000000:15B3 0100007F:C000 01 00000000:00000000 00:00000000 00000000 0 0 1 1 0
`
	if hasListener(established, adbPortHex) {
		t.Error("an established socket was mistaken for a listener")
	}
	if hasListener(procNetTCP, ":1F41") {
		t.Error("hasListener matched a port that is not in the table")
	}
	if hasListener("", adbPortHex) {
		t.Error("hasListener matched an empty table")
	}
	if hasListener("  sl  local_address\n", adbPortHex) {
		t.Error("hasListener matched a header line")
	}
}

func TestHasListenerDoesNotMatchPartialPort(t *testing.T) {
	other := `   0: 00000000:215B3 00000000:0000 0A 0 0 0 0 0 1 1 0
`
	if hasListener(other, ":5B3") {
		t.Error("hasListener matched a partial port")
	}
}

func TestADBModeNames(t *testing.T) {
	for mode, want := range map[ADBMode]string{
		ADBOff:      "Off",
		ADBInsecure: "Insecure",
		ADBSecure:   "Secure",
	} {
		if got := mode.String(); got != want {
			t.Errorf("ADBMode(%d).String() = %q, want %q", mode, got, want)
		}
	}
}

func TestParseADBModeRoundTrips(t *testing.T) {
	for _, mode := range []ADBMode{ADBOff, ADBInsecure, ADBSecure} {
		got, ok := ParseADBMode(mode.String())
		if !ok || got != mode {
			t.Errorf("ParseADBMode(%q) = %v, %v; want %v, true", mode.String(), got, ok, mode)
		}
	}
}

func TestParseADBModeRejectsAnythingElse(t *testing.T) {
	for _, s := range []string{"", "off", "OFF", "secure", "on", "true", "Disabled"} {
		if got, ok := ParseADBMode(s); ok {
			t.Errorf("ParseADBMode(%q) = %v, true; want refused", s, got)
		}
	}
}

func TestInstallADBKeyLandsReadableByAdbd(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	dir := t.TempDir()
	src, keyDir := filepath.Join(dir, "src"), filepath.Join(dir, "misc", "adb")
	if err := os.WriteFile(src, []byte("ssh-rsa AAAA... user@host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer swapADBKeyPaths(src, keyDir, filepath.Join(keyDir, "adb_keys"))()

	_ = installADBKey()

	for path, want := range map[string]os.FileMode{
		keyDir:                            0o750,
		filepath.Join(keyDir, "adb_keys"): 0o640,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %04o, want %04o (adbd runs as shell and must read it)", path, got, want)
		}
	}
}

func TestInstallADBKeyFixesAnExistingTree(t *testing.T) {
	dir := t.TempDir()
	src, keyDir := filepath.Join(dir, "src"), filepath.Join(dir, "misc", "adb")
	if err := os.WriteFile(src, []byte("ssh-rsa AAAA... user@host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(keyDir, "adb_keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer swapADBKeyPaths(src, keyDir, keyFile)()

	_ = installADBKey()

	if info, _ := os.Stat(keyFile); info.Mode().Perm() != 0o640 {
		t.Errorf("existing key left at %04o, want 0640", info.Mode().Perm())
	}
	if info, _ := os.Stat(keyDir); info.Mode().Perm() != 0o750 {
		t.Errorf("existing directory left at %04o, want 0750", info.Mode().Perm())
	}
}

func swapADBKeyPaths(src, dir, file string) func() {
	oldSrc, oldDir, oldFile := ADBKeySource, adbKeyDir, adbKeyFile
	ADBKeySource, adbKeyDir, adbKeyFile = src, dir, file
	return func() { ADBKeySource, adbKeyDir, adbKeyFile = oldSrc, oldDir, oldFile }
}

// netd rebuilds the INPUT chain and drops what it finds, so the rule has to be
// put back -- but only for a port something is answering on. The question is
// whether adbd is listening rather than what CurrentADBMode reports: that
// answer folds the rule into itself and says Off once the rule is gone, so a
// re-assert gated on it would stop at the moment it is needed.
func TestHoldADBOpenAsksWhetherADBDIsListening(t *testing.T) {
	for _, tt := range []struct {
		name      string
		listening bool
		wantCalls int
	}{
		{"nothing listening, nothing to hold open", false, 0},
		{"adbd listening, so the rule goes back", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer swapADBListens(tt.listening)()
			calls := 0
			was := iptablesRun
			iptablesRun = func(args ...string) ([]byte, error) {
				calls++
				return nil, nil
			}
			defer func() { iptablesRun = was }()

			if err := HoldADBOpen(); err != nil {
				t.Fatalf("HoldADBOpen: %v", err)
			}
			// AllowTCP checks and returns when the rule is there, so one call
			// is the whole of it: what this counts is whether iptables was
			// reached at all.
			if calls != tt.wantCalls {
				t.Errorf("iptables was run %d times, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func swapADBListens(listening bool) func() {
	was := adbListens
	adbListens = func() bool { return listening }
	return func() { adbListens = was }
}

// adbd's network transport binds the wildcard. Something else answering on
// loopback is not it, and taking it for adbd opens the firewall to the subnet
// for a port nothing off the device can reach.
func TestHasListenerIgnoresALoopbackOnlyPort(t *testing.T) {
	loopback := `   0: 0100007F:15B3 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 1 1 0
`
	if hasListener(loopback, adbPortHex) {
		t.Error("a socket bound to 127.0.0.1 was taken for adbd listening to the network")
	}
	wildcard := `   0: 00000000:15B3 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 1 1 0
`
	if !hasListener(wildcard, adbPortHex) {
		t.Error("adbd on the wildcard address was missed")
	}
}

// The loop ends on iptables reporting that it matched nothing. One that answered
// 0 for a delete that removed nothing would spin holding chainMu, which stops
// tcp/6053 being re-asserted and takes the API away at netd's next rebuild.
func TestDenyTCPGivesUpRatherThanSpinning(t *testing.T) {
	was := iptablesRun
	calls := 0
	iptablesRun = func(args ...string) ([]byte, error) {
		calls++
		return nil, nil // a -D that reports success and removes nothing
	}
	defer func() { iptablesRun = was }()

	done := make(chan error, 1)
	go func() { done <- DenyTCP(ADBPort) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("DenyTCP reported the port closed after a delete that never matched")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("DenyTCP did not return; it ran iptables %d times", calls)
	}
}
