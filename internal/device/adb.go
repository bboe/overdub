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

// Where adbd is asked to listen, and the port the firewall rule names.
const ADBPort = 5555

// Magisk's, because ro.* properties are read-only to setprop once init has
// started and resetprop writes them anyway.
const resetprop = "/sbin/resetprop"

var (
	ADBKeySource = "/data/local/bin/adb_keys"
	adbKeyDir    = "/data/misc/adb"
	adbKeyFile   = "/data/misc/adb/adb_keys"
)

// adbd drops to shell, so the key it authenticates against has to be readable
// by that uid and by nothing else useful.
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

// The property tools are forked from the sensor poll as well as from the adb
// worker, and that poll is serial, so one that never returned would cost every
// reading behind it rather than this one. Bounded the way the volume read is,
// and for the same second reason: killing the child does not close a
// descendant's copy of the pipe, which is what the wait delay is for.
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

// Whether the port is both listening and permitted, and whether that could be
// established at all. The rule check runs iptables, which waits on the lock
// netd holds constantly, so it can fail on a working device -- and answering
// "open" there reports adb as reachable by the whole subnet on no evidence,
// which is the reading every other source in this tree refuses to invent.
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
		// Bound to every address rather than to one. adbd's network transport
		// takes the wildcard, so something else answering on loopback is not
		// it -- and taking it for adbd opens the firewall to the subnet for a
		// port nothing off the device can reach.
		if strings.Trim(strings.TrimSuffix(local, portHex), "0") != "" {
			continue
		}
		return true
	}
	return false
}

// The mode, and whether the device could be asked. A false there is not Off:
// the caller publishes nothing rather than a position, so Home Assistant keeps
// what it had instead of being told the port is closed on a reading that failed.
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

// Read through a variable so a test can answer for a getprop this host has not
// got.
var readProp = func(name string) (string, error) {
	cmd, cancel := propCmd("/system/bin/getprop", name)
	defer cancel()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Whether ro.adb.secure is 1, and whether that could be established at all. The
// second return is the point: a getprop that failed is not a device with
// authentication switched off. Collapsing the two would publish Insecure for a
// Dot that is in Secure -- inventing the weaker of two security postures on no
// evidence, which is what adbReachable above refuses to do -- and would send
// the settle read down the fail-closed branch, shutting adb off on a device
// that was already exactly where it was asked to be.
func adbSecureEnforced() (secure, known bool) {
	out, err := readProp("ro.adb.secure")
	if err != nil {
		return false, false
	}
	return out == "1", true
}

// HoldADBOpen puts the rule back if adbd is listening, which is what netd's
// rebuild of the INPUT chain takes away. The question it asks is whether adbd
// is listening rather than what CurrentADBMode says: that answer folds the rule
// into itself and reports Off once the rule is gone, so a re-assert gated on it
// would stop exactly when it is needed.
//
// Under the same lock as SetADBMode, so a rule cannot be put back by a decision
// taken a moment before the port was closed. It is not the server's lock: this
// waits on iptables, which waits on netd.
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
		// The rule goes after the properties rather than before them, and that
		// order is the whole of the close. A step here can fail -- these are
		// forks with a budget -- and a rule taken out in front of a property
		// that did not take leaves adbd listening with nothing in the chain to
		// say so. The sensor poll re-asserts on whether adbd is listening,
		// which cannot tell that apart from a port somebody wants open, so it
		// puts the rule back within the minute and the Dot the operator asked
		// to close is serving the subnet again. Failing before the rule is
		// touched leaves the device where it was, which the reading then
		// reports truthfully.
		if err := setProp("service.adb.tcp.port", "-1"); err != nil {
			return err
		}
		if err := restartADBD(); err != nil {
			return err
		}
		// Reported rather than returned early, and neither aborts the close:
		// the port is shut either way, and ro.adb.secure left at 1 is stricter
		// than what was asked for rather than weaker.
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

// DenyADB closes the port under the lock SetADBMode and HoldADBOpen share.
// DenyTCP alone takes only the chain lock, so a re-assert that read "listening"
// a moment before the close can put its rule back afterwards -- and it can,
// since AllowTCP waits on netd's xtables lock for longer than the settle the
// caller spent. The chain would then keep an ACCEPT for a port the select
// truthfully reports as closed, and nothing tidies a chain that is not ours.
func DenyADB() error {
	adbMu.Lock()
	defer adbMu.Unlock()

	return DenyTCP(ADBPort)
}
