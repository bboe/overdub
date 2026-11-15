package device

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
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

func adbKeyLine(t *testing.T, seed byte, name string) string {
	t.Helper()
	b := make([]byte, adbKeyRSALen)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(b) + " " + name
}

func fakeKeyDir(t *testing.T) {
	t.Helper()
	wasDir, wasFile, wasSystem := adbKeyDir, adbKeyFile, adbSystemKey
	t.Cleanup(func() { adbKeyDir, adbKeyFile, adbSystemKey = wasDir, wasFile, wasSystem })
	root := t.TempDir()
	adbKeyDir = filepath.Join(root, "adb")
	adbKeyFile = filepath.Join(adbKeyDir, "adb_keys")
	adbSystemKey = filepath.Join(root, "adb_keys")
}

func TestSettingTheKeysReplacesThemWhole(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the key file is owned by system:shell, so writing it needs root")
	}
	fakeKeyDir(t)
	defer syscall.Umask(syscall.Umask(0o077))
	one, two := adbKeyLine(t, 1, "one@host"), adbKeyLine(t, 2, "two@host")
	if n, err := SetADBKeys(one + "\r\n\n" + two + "\n"); err != nil || n != 2 {
		t.Fatalf("SetADBKeys = %d, %v; want 2 keys", n, err)
	}
	if n, err := SetADBKeys(two); err != nil || n != 1 {
		t.Fatalf("SetADBKeys = %d, %v; want 1 key", n, err)
	}
	b, err := os.ReadFile(adbKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != two+"\n" {
		t.Errorf("the file holds %q, want the second call's key alone: a call states the"+
			" whole set, so a key taken out of it stops working", b)
	}
	st, _ := os.Stat(adbKeyFile)
	if st.Mode().Perm() != 0o640 {
		t.Errorf("the file is %v under the daemon's 077 umask, want 0640: adbd reads it by"+
			" group, and on overdub-test a bare write landed 0600", st.Mode().Perm())
	}
	if err := os.WriteFile(adbSystemKey, []byte(adbKeyLine(t, 9, "rooter@mac")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rooting, set, ok := ADBKeyNames(); !ok || !slices.Equal(rooting, []string{"rooter@mac"}) ||
		!slices.Equal(set, []string{"two@host"}) {
		t.Errorf("ADBKeyNames = %v, %v, %v; want [rooter@mac], [two@host]: the rooting"+
			" computer always gets in, so the sensor must show it", rooting, set, ok)
	}
	st, _ = os.Stat(adbKeyDir)
	if st.Mode()&os.ModeSetgid == 0 || st.Mode().Perm() != 0o750 {
		t.Errorf("the directory is %v, want 02750 as init makes it: a write that clears"+
			" setgid leaves later files out of group shell", st.Mode())
	}
	for _, empty := range []string{"", " \n\t\n"} {
		if _, err := SetADBKeys(empty); !errors.Is(err, ErrNoADBKeys) {
			t.Errorf("SetADBKeys(%q) gave %v, want ErrNoADBKeys: a form run before the"+
				" paste would revoke every machine added", empty, err)
		}
	}
	if b, _ := os.ReadFile(adbKeyFile); string(b) != two+"\n" {
		t.Error("an empty call changed the keys")
	}
	if n, err := SetADBKeys(" none \n"); err != nil || n != 0 {
		t.Fatalf("none gave %d, %v", n, err)
	}
	if _, err := os.Stat(adbKeyFile); !errors.Is(err, os.ErrNotExist) {
		t.Error("none left the file, so its keys still let a host in")
	}
	if rooting, set, ok := ADBKeyNames(); !ok || len(set) != 0 || len(rooting) != 1 {
		t.Errorf("after none, ADBKeyNames = %v, %v, %v; want the rooting key alone", rooting, set, ok)
	}
}

func TestARefusalNamesTheLineAsPasted(t *testing.T) {
	fakeKeyDir(t)
	key := adbKeyLine(t, 4, "a@host")
	_, err := SetADBKeys(key + "\n\n" + key + "\r\n\nnot a key\n")
	if err == nil || !strings.Contains(err.Error(), "line 5") {
		t.Errorf("got %v, want line 5: blank lines count, or the user fixes the wrong key", err)
	}
}

func TestATabBeforeTheNameIsAccepted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the key file is owned by system:shell, so writing it needs root")
	}
	fakeKeyDir(t)
	key := strings.Replace(adbKeyLine(t, 5, "tab@host"), " ", "\t", 1)
	if n, err := SetADBKeys(key); err != nil || n != 1 {
		t.Errorf("a key with a tab before its name gave %d, %v", n, err)
	}
}

func TestAnythingButAKeyIsRefusedAndChangesNothing(t *testing.T) {
	fakeKeyDir(t)
	for _, text := range []string{
		"ssh-rsa AAAAB3NzaC1yc2E whoever",
		"not base64 at all",
		base64.StdEncoding.EncodeToString(make([]byte, 100)) + " short@key",
		strings.Repeat(adbKeyLine(t, 3, "x")+"\n", maxADBKeys+1),
	} {
		if _, err := SetADBKeys(text); err == nil {
			t.Errorf("SetADBKeys(%.40q...) was accepted", text)
		}
	}
	if _, err := os.Stat(adbKeyDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused set touched the key directory")
	}
}

func TestKeyNamesAreValidUTF8(t *testing.T) {
	fakeKeyDir(t)
	if err := os.WriteFile(adbSystemKey, []byte(adbKeyLine(t, 9, "caf\xe9@mac")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rooting, _, _ := ADBKeyNames()
	if len(rooting) != 1 || !utf8.ValidString(rooting[0]) {
		t.Errorf("ADBKeyNames = %q; Home Assistant refuses a state that is not UTF-8", rooting)
	}
}
