package device

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestDenyTCPGivesUpRatherThanSpinning(t *testing.T) {
	was := iptablesRun
	calls := 0
	iptablesRun = func(args ...string) ([]byte, error) {
		calls++
		return nil, nil
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

func TestAFailedSecureReadIsNoReading(t *testing.T) {
	defer func(r func(string) (string, error), l func() bool, i func(...string) ([]byte, error)) {
		readProp, adbListens, iptablesRun = r, l, i
	}(readProp, adbListens, iptablesRun)

	adbListens = func() bool { return true }
	iptablesRun = func(...string) ([]byte, error) { return nil, nil }
	readProp = func(string) (string, error) { return "", errors.New("getprop: killed") }

	if mode, known := CurrentADBMode(); known {
		t.Errorf("CurrentADBMode() = %v, true; want a reading nobody took", mode)
	}

	readProp = func(string) (string, error) { return "1", nil }
	if mode, known := CurrentADBMode(); !known || mode != ADBSecure {
		t.Errorf("CurrentADBMode() = %v, %v; want Secure, true", mode, known)
	}
}

func TestACloseThatFailedLeavesTheRuleAlone(t *testing.T) {
	defer func(s func(string, string) error, i func(...string) ([]byte, error)) {
		setProp, iptablesRun = s, i
	}(setProp, iptablesRun)

	var mu sync.Mutex
	var ran [][]string
	iptablesRun = func(args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, args)
		return nil, nil
	}
	setProp = func(key, _ string) error { return fmt.Errorf("setprop %s: killed", key) }

	if err := SetADBMode(ADBOff); err == nil {
		t.Fatal("SetADBMode(Off) reported success with every property write failing")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, args := range ran {
		for _, a := range args {
			if a == "-D" {
				t.Fatalf("the rule was deleted by a close that failed: %v", ran)
			}
		}
	}
}

func TestDenyADBWaitsForTheModeLock(t *testing.T) {
	defer func(i func(...string) ([]byte, error)) { iptablesRun = i }(iptablesRun)
	iptablesRun = func(...string) ([]byte, error) { return nil, nil }

	adbMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = DenyADB()
	}()

	select {
	case <-done:
		adbMu.Unlock()
		t.Fatal("DenyADB closed the port while the mode lock was held")
	case <-time.After(50 * time.Millisecond):
	}

	adbMu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DenyADB never ran once the mode lock was free")
	}
}
