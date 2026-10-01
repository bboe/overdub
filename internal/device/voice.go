package device

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	speechHostProp = "persist.amazon.scl.host"
	speechPortProp = "persist.amazon.scl.port"

	speechApp = "amazon.speech.sim"

	mountTimeout = 15 * time.Second

	propPoll = 100 * time.Millisecond
)

var propWaitFor = 5 * time.Second

var procRoot = "/proc"

// RedirectSpeech reports whether it moved the endpoint. It is left alone when
// it already points here, because the app is restarted to read it and a daemon
// that restarts the app every time it starts keeps Alexa dead in a crash loop.
func RedirectSpeech(host, port string) (bool, error) {
	want := []struct{ name, value string }{
		{speechHostProp, host},
		{speechPortProp, port},
	}
	same := true
	for _, prop := range want {
		got, err := readProp(prop.name)
		if err != nil {
			return false, err
		}
		if got != prop.value {
			same = false
		}
	}
	if same {
		return false, nil
	}
	for _, prop := range want {
		if err := setProp(prop.name, prop.value); err != nil {
			_ = ReleaseSpeech()
			return false, err
		}
		if err := awaitProp(prop.name, prop.value); err != nil {
			_ = ReleaseSpeech()
			return false, err
		}
	}
	return true, nil
}

func awaitProp(name, want string) error {
	var got string
	for deadline := time.Now().Add(propWaitFor); ; {
		var err error
		if got, err = readProp(name); err != nil {
			return err
		}
		if got == want {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(propPoll)
	}
	return fmt.Errorf("%s read back as %q, not %q", name, got, want)
}

func ReleaseSpeech() error {
	for _, name := range []string{speechHostProp, speechPortProp} {
		if err := setProp(name, ""); err != nil {
			return err
		}
		got, err := readProp(name)
		if err != nil {
			return err
		}
		if got != "" {
			return fmt.Errorf("%s read back as %q, not empty", name, got)
		}
	}
	return nil
}

func RestartSpeech() error {
	pid, err := speechPID()
	if err != nil || pid == 0 {
		return err
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

func speechPID() (int, error) {
	names, err := filepath.Glob(filepath.Join(procRoot, "[0-9]*", "cmdline"))
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if string(bytes.TrimRight(raw, "\x00")) != speechApp {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(name)))
		if err != nil {
			continue
		}
		return pid, nil
	}
	return 0, nil
}

func Trusted(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func Trust(path string, pem []byte) error {
	if err := remount("rw"); err != nil {
		return err
	}
	defer func() {
		if err := remount("ro"); err != nil {
			fmt.Fprintf(os.Stderr, "leaving /system writable: %v\n", err)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		return err
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, pem) {
		return fmt.Errorf("%s did not read back as it was written", path)
	}
	return nil
}

func Untrust(path string) error {
	if !Trusted(path) {
		return nil
	}
	if err := remount("rw"); err != nil {
		return err
	}
	defer remount("ro")
	if err := os.Remove(path); err != nil {
		return err
	}
	if Trusted(path) {
		return fmt.Errorf("%s is still there", path)
	}
	return nil
}

func slowCmd(name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	return cmd, cancel
}

var remount = func(how string) error {
	cmd, cancel := slowCmd("/system/bin/mount", "-o", how+",remount", "/system")
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remounting /system %s: %w: %s", how, err, bytes.TrimSpace(out))
	}
	return nil
}
