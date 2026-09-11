package device

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const ADBPort = 5555

const resetprop = "/sbin/resetprop"

var (
	ADBKeySource = "/data/local/bin/adb_keys"
	adbKeyDir    = "/data/misc/adb"
	adbKeyFile   = "/data/misc/adb/adb_keys"
)

const (
	aidSystem = 1000
	aidShell  = 2000
)

type ADBMode int

const (
	ADBOff ADBMode = iota
	ADBInsecure
	ADBSecure
)

var adbModeName = [...]string{ADBOff: "Off", ADBInsecure: "Insecure", ADBSecure: "Secure"}

func (m ADBMode) String() string {
	if m < 0 || int(m) >= len(adbModeName) {
		return "Off"
	}
	return adbModeName[m]
}

func ParseADBMode(s string) (ADBMode, bool) {
	for i, name := range adbModeName {
		if name == s {
			return ADBMode(i), true
		}
	}
	return ADBOff, false
}

func ADBSecureAvailable() bool {
	_, err := os.Stat(ADBKeySource)
	return err == nil
}

var propBudget = 2 * time.Second

func propCmd(name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), propBudget)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	return cmd, cancel
}

const tcpStListen = "0A" // TCP_LISTEN in /proc/net/tcp

var adbPortHex = fmt.Sprintf(":%04X", ADBPort)

func ADBListening() bool {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		out, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if hasListener(string(out), adbPortHex) {
			return true
		}
	}
	return false
}

func adbReachable() (open, known bool) {
	if !adbListens() {
		return false, true
	}
	permitted, err := present(inputRule(ADBPort))
	if err != nil {
		return false, false
	}
	return permitted, true
}

func hasListener(table, portHex string) bool {
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != tcpStListen {
			continue
		}
		local := strings.ToUpper(f[1])
		if !strings.HasSuffix(local, portHex) {
			continue
		}
		if strings.Trim(strings.TrimSuffix(local, portHex), "0") != "" {
			continue
		}
		return true
	}
	return false
}

func CurrentADBMode() (ADBMode, bool) {
	open, known := adbReachable()
	if !known {
		return ADBOff, false
	}
	if !open {
		return ADBOff, true
	}
	secure, known := adbSecureEnforced()
	if !known {
		return ADBOff, false
	}
	if secure {
		return ADBSecure, true
	}
	return ADBInsecure, true
}

var readProp = func(name string) (string, error) {
	cmd, cancel := propCmd("/system/bin/getprop", name)
	defer cancel()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func adbSecureEnforced() (secure, known bool) {
	out, err := readProp("ro.adb.secure")
	if err != nil {
		return false, false
	}
	return out == "1", true
}

var adbListens = ADBListening

func HoldADBOpen() error {
	adbMu.Lock()
	defer adbMu.Unlock()

	if !adbListens() {
		return nil
	}
	return AllowTCP(ADBPort)
}

var adbMu sync.Mutex

func SetADBMode(mode ADBMode) error {
	adbMu.Lock()
	defer adbMu.Unlock()

	if mode == ADBOff {
		if err := setProp("service.adb.tcp.port", "-1"); err != nil {
			return err
		}
		if err := restartADBD(); err != nil {
			return err
		}
		if err := DenyTCP(ADBPort); err != nil {
			return err
		}
		return clearADBSecure()
	}

	if mode == ADBSecure {
		if err := installADBKey(); err != nil {
			return err
		}
		if err := setResetprop("ro.adb.secure", "1"); err != nil {
			return err
		}
		secure, known := adbSecureEnforced()
		if !known {
			return fmt.Errorf("ro.adb.secure could not be read after resetprop")
		}
		if !secure {
			return fmt.Errorf("ro.adb.secure is not 1 after resetprop")
		}
	} else if err := clearADBSecure(); err != nil {
		return err
	}

	if err := setProp("service.adb.tcp.port", fmt.Sprint(ADBPort)); err != nil {
		return err
	}
	if err := restartADBD(); err != nil {
		return err
	}
	return AllowTCP(ADBPort)
}

func installADBKey() error {
	key, err := os.ReadFile(ADBKeySource)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ADBKeySource, err)
	}
	if err := os.MkdirAll(adbKeyDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", adbKeyDir, err)
	}
	if err := os.WriteFile(adbKeyFile, key, 0o640); err != nil {
		return fmt.Errorf("writing %s: %w", adbKeyFile, err)
	}
	if err := os.Chmod(adbKeyDir, 0o750); err != nil {
		return fmt.Errorf("mode on %s: %w", adbKeyDir, err)
	}
	if err := os.Chmod(adbKeyFile, 0o640); err != nil {
		return fmt.Errorf("mode on %s: %w", adbKeyFile, err)
	}
	if err := os.Chown(adbKeyDir, aidSystem, aidShell); err != nil {
		return fmt.Errorf("owning %s: %w", adbKeyDir, err)
	}
	if err := os.Chown(adbKeyFile, aidSystem, aidShell); err != nil {
		return fmt.Errorf("owning %s: %w", adbKeyFile, err)
	}
	return nil
}

func clearADBSecure() error {
	clear, cancel := propCmd(resetprop, "--delete", "ro.adb.secure")
	_ = clear.Run()
	cancel()
	secure, known := adbSecureEnforced()
	if !known {
		return fmt.Errorf("ro.adb.secure could not be read after resetprop --delete")
	}
	if secure {
		return fmt.Errorf("ro.adb.secure is still 1 after resetprop --delete")
	}
	return nil
}

func setResetprop(key, value string) error {
	cmd, cancel := propCmd(resetprop, key, value)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("resetprop %s: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

var setProp = func(key, value string) error {
	cmd, cancel := propCmd("/system/bin/setprop", key, value)
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("setprop %s: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func restartADBD() error { return setProp("ctl.restart", "adbd") }

func DenyADB() error {
	adbMu.Lock()
	defer adbMu.Unlock()

	return DenyTCP(ADBPort)
}
