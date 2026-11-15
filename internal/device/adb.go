package device

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const ADBPort = 5555

var (
	adbKeyDir    = "/data/misc/adb"
	adbKeyFile   = "/data/misc/adb/adb_keys"
	adbSystemKey = "/adb_keys"
)

const (
	aidSystem = 1000
	aidShell  = 2000

	maxADBKeys   = 16
	adbKeyRSALen = 524
)

const ADBKeysClear = "none"

var (
	adbKeysMu    sync.Mutex
	ErrNoADBKeys = fmt.Errorf("no keys given; send %q to remove them all", ADBKeysClear)
)

func SetADBKeys(text string) (int, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, ErrNoADBKeys
	}
	var keys []string
	if trimmed != ADBKeysClear {
		for i, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if b, err := base64.StdEncoding.DecodeString(fields[0]); err != nil || len(b) != adbKeyRSALen {
				return 0, fmt.Errorf("line %d is not an adb public key: want the %d-byte RSA"+
					" key adbkey.pub holds, base64, then an optional name", i+1, adbKeyRSALen)
			}
			keys = append(keys, strings.Join(fields, " "))
		}
		if len(keys) == 0 {
			return 0, ErrNoADBKeys
		}
		if len(keys) > maxADBKeys {
			return 0, fmt.Errorf("%d keys, over the %d allowed", len(keys), maxADBKeys)
		}
	}
	adbKeysMu.Lock()
	defer adbKeysMu.Unlock()
	if len(keys) == 0 {
		if err := os.Remove(adbKeyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		return 0, nil
	}
	if err := writeADBKeys(strings.Join(keys, "\n") + "\n"); err != nil {
		return 0, err
	}
	return len(keys), nil
}

func writeADBKeys(body string) error {
	if err := os.MkdirAll(adbKeyDir, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(adbKeyDir, 0o750|os.ModeSetgid); err != nil {
		return err
	}
	if err := os.Chown(adbKeyDir, aidSystem, aidShell); err != nil {
		return err
	}
	tmp := adbKeyFile + ".new"
	err := writeSynced(tmp, body)
	if err == nil {
		err = os.Rename(tmp, adbKeyFile)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	dir, err := os.Open(adbKeyDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func writeSynced(path, body string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o640); err != nil {
		f.Close()
		return err
	}
	if err := f.Chown(aidSystem, aidShell); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func ADBKeyNames() (rooting, set []string, ok bool) {
	rooting, rootingOK := keyNames(adbSystemKey)
	set, setOK := keyNames(adbKeyFile)
	return rooting, set, rootingOK && setOK
}

func keyNames(path string) ([]string, bool) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var names []string
	for _, line := range strings.Split(strings.ToValidUTF8(string(b), "?"), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) > 1:
			names = append(names, strings.Join(fields[1:], " "))
		case len(fields) == 1:
			names = append(names, "unnamed")
		}
	}
	return names, true
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

var readProp = func(name string) (string, error) {
	cmd, cancel := propCmd("/system/bin/getprop", name)
	defer cancel()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

var adbListens = ADBListening

func HoldADBOpen() error {
	if !adbListens() {
		return nil
	}
	return AllowTCP(ADBPort)
}

var setProp = func(key, value string) error {
	cmd, cancel := propCmd("/system/bin/setprop", key, value)
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("setprop %s: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}
