package device

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func fakeProc(t *testing.T) {
	t.Helper()
	oldProc, oldWait, oldRead, oldSet := procDir, readerWait, readProp, setProp
	t.Cleanup(func() { procDir, readerWait, readProp, setProp = oldProc, oldWait, oldRead, oldSet })
	procDir = t.TempDir()
	readerWait = 300 * time.Millisecond
}

func fakeProcess(t *testing.T, pid, exe string, fds ...string) {
	t.Helper()
	dir := filepath.Join(procDir, pid)
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(exe+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, target := range fds {
		if err := os.Symlink(target, filepath.Join(dir, "fd", string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
}

const clone = "/dev/input/event3"

func TestAReaderOnTheDeadCloneIsRestartedOntoTheLiveOne(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "485", "/system/bin/acebuttond", clone+" (deleted)")
	fakeProcess(t, "246", "/system/bin/ace_usagestat_protod", clone)
	readProp = func(string) (string, error) { return "running", nil }
	var set []string
	setProp = func(key, value string) error {
		set = append(set, key+"="+value)
		os.RemoveAll(filepath.Join(procDir, "485"))
		fakeProcess(t, "2817", "/system/bin/acebuttond", clone)
		return nil
	}
	restarted, err := RestartInputReaders(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restarted, []string{"acebuttond"}) ||
		!slices.Equal(set, []string{"ctl.restart=acebuttond"}) {
		t.Errorf("restarted %v by %v, want acebuttond alone: the other already reads the"+
			" live clone, and a restart would drop a press for nothing", restarted, set)
	}
}

func TestOnlyARunningServiceIsRestarted(t *testing.T) {
	fakeProc(t)
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "stopped", nil
		}
		return "", nil
	}
	setProp = func(key, value string) error {
		t.Errorf("setprop %s %s: a stopped service was started, and one this Dot lacks"+
			" was asked for", key, value)
		return nil
	}
	if restarted, err := RestartInputReaders(clone); err != nil || len(restarted) > 0 {
		t.Errorf("restarted %v, %v; want nothing", restarted, err)
	}
}

func TestARestartIsReadBackNotAssumed(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "485", "/system/bin/acebuttond", clone+" (deleted)")
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "running", nil
		}
		return "", nil
	}
	setProp = func(string, string) error { return nil }
	if _, err := RestartInputReaders(clone); err == nil {
		t.Error("a restart init ignored was reported as done; setprop exits 0 either way")
	}
}

func TestAFailedReadIsNamedNotTakenForNoService(t *testing.T) {
	fakeProc(t)
	readProp = func(string) (string, error) { return "", errors.New("getprop: killed") }
	setProp = func(string, string) error { return nil }
	if _, err := RestartInputReaders(clone); err == nil {
		t.Error("a getprop that failed read as a Dot without the service")
	}
}

func TestAFailedRestartIsNamed(t *testing.T) {
	fakeProc(t)
	readProp = func(string) (string, error) { return "running", nil }
	setProp = func(string, string) error { return errors.New("killed") }
	if _, err := RestartInputReaders(clone); err == nil {
		t.Error("a restart that failed reported none")
	}
}

func TestOneReaderFailingDoesNotStrandTheOther(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "485", "/system/bin/acebuttond", clone+" (deleted)")
	fakeProcess(t, "246", "/system/bin/ace_usagestat_protod", clone+" (deleted)")
	readProp = func(string) (string, error) { return "running", nil }
	setProp = func(_, service string) error {
		if service == "acebuttond" {
			return errors.New("killed")
		}
		os.RemoveAll(filepath.Join(procDir, "246"))
		fakeProcess(t, "300", "/system/bin/ace_usagestat_protod", clone)
		return nil
	}
	restarted, err := RestartInputReaders(clone)
	if err == nil || !slices.Equal(restarted, []string{"usagestat_protod"}) {
		t.Errorf("restarted %v with %v, want usagestat_protod and acebuttond's error: the"+
			" two are independent, and the second would poll the dead clone till reboot",
			restarted, err)
	}
}
