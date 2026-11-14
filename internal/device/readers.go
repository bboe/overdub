package device

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type inputReader struct {
	service string
	exe     string
}

var (
	inputReaders = []inputReader{
		{"acebuttond", "/system/bin/acebuttond"},
		{"usagestat_protod", "/system/bin/ace_usagestat_protod"},
	}
	procDir    = "/proc"
	readerWait = 30 * time.Second
	readerPoll = 100 * time.Millisecond
)

func RestartInputReaders(clone string) (restarted []string, err error) {
	var errs []error
	for _, r := range inputReaders {
		ok, err := restartReader(r, clone)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			restarted = append(restarted, r.service)
		}
	}
	return restarted, errors.Join(errs...)
}

func restartReader(r inputReader, clone string) (bool, error) {
	state, err := readProp("init.svc." + r.service)
	if err != nil {
		return false, fmt.Errorf("reading init.svc.%s: %w", r.service, err)
	}
	if state != "running" || holds(r.exe, clone) {
		return false, nil
	}
	if err := setProp("ctl.restart", r.service); err != nil {
		return false, fmt.Errorf("restarting %s: %w", r.service, err)
	}
	deadline := time.Now().Add(readerWait)
	for !holds(r.exe, clone) {
		if time.Now().After(deadline) {
			return false, fmt.Errorf("%s did not open %s within %v of a restart",
				r.service, clone, readerWait)
		}
		time.Sleep(readerPoll)
	}
	return true, nil
}

func holds(exe, node string) bool {
	pids, _ := filepath.Glob(filepath.Join(procDir, "[0-9]*"))
	for _, pid := range pids {
		argv, err := os.ReadFile(filepath.Join(pid, "cmdline"))
		if err != nil {
			continue
		}
		if first, _, _ := bytes.Cut(argv, []byte{0}); string(first) != exe {
			continue
		}
		fds, _ := filepath.Glob(filepath.Join(pid, "fd", "*"))
		for _, fd := range fds {
			if target, err := os.Readlink(fd); err == nil && target == node {
				return true
			}
		}
	}
	return false
}
