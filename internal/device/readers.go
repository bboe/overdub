package device

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
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
	inputDir   = "/dev/input"
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
	if state != "running" || settled(r.exe, clone) {
		return false, nil
	}
	if err := setProp("ctl.restart", r.service); err != nil {
		return false, fmt.Errorf("restarting %s: %w", r.service, err)
	}
	deadline := time.Now().Add(readerWait)
	for !settled(r.exe, clone) {
		if time.Now().After(deadline) {
			return false, fmt.Errorf("%s did not hold %s, and no destroyed input node, within %v of a restart",
				r.service, clone, readerWait)
		}
		time.Sleep(readerPoll)
	}
	return true, nil
}

func settled(exe, clone string) bool {
	live, dead := held(exe, clone)
	return live && !dead
}

func held(exe, clone string) (live, dead bool) {
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
			target, err := os.Readlink(fd)
			if err != nil {
				continue
			}
			live = live || target == clone
			dead = dead || strings.HasPrefix(target, inputDir+"/") && strings.HasSuffix(target, " (deleted)")
		}
	}
	return live, dead
}

func WatchInputNodes(ready func(), gone func(names []string)) error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("watching %s: %w", inputDir, err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, inputDir, syscall.IN_DELETE); err != nil {
		return fmt.Errorf("watching %s: %w", inputDir, err)
	}
	ready()
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("watching %s: %w", inputDir, err)
		}
		names, ended := inotifyNames(buf[:n])
		if len(names) > 0 {
			gone(names)
		}
		if ended {
			return fmt.Errorf("watching %s: the watch was removed", inputDir)
		}
	}
}

func inotifyNames(b []byte) (names []string, ended bool) {
	for len(b) >= syscall.SizeofInotifyEvent {
		ev := (*syscall.InotifyEvent)(unsafe.Pointer(&b[0]))
		end := syscall.SizeofInotifyEvent + int(ev.Len)
		if end > len(b) {
			break
		}
		switch {
		case ev.Mask&syscall.IN_IGNORED != 0:
			ended = true
		case ev.Mask&syscall.IN_Q_OVERFLOW != 0:
			names = append(names, "")
		case ev.Mask&syscall.IN_DELETE != 0:
			name, _, _ := bytes.Cut(b[syscall.SizeofInotifyEvent:end], []byte{0})
			names = append(names, string(name))
		}
		b = b[end:]
	}
	return names, ended
}
