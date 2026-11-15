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
	procDir     = "/proc"
	inputDir    = "/dev/input"
	readerWait  = 30 * time.Second
	readerPoll  = 100 * time.Millisecond
	readerStill = 500 * time.Millisecond
)

const (
	transientNode = "AVRCP"
	asidePrefix   = ".overdub-input-"
)

func RestartInputReaders(clone string, nodeName func(path string) (string, error)) (restarted []string, err error) {
	var errs []error
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(inputDir), asidePrefix+"*"))
	for _, aside := range leftovers {
		if err := restoreNode(aside); err != nil {
			errs = append(errs, err)
		}
	}
	for _, r := range inputReaders {
		ok, err := restartReader(r, clone, nodeName)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			restarted = append(restarted, r.service)
		}
	}
	return restarted, errors.Join(errs...)
}

func restartReader(r inputReader, clone string, nodeName func(path string) (string, error)) (restarted bool, err error) {
	state, err := readProp("init.svc." + r.service)
	if err != nil {
		return false, fmt.Errorf("reading init.svc.%s: %w", r.service, err)
	}
	if state != "running" || settled(r.exe, clone) {
		return false, nil
	}
	restore := hideTransient(clone, nodeName)
	defer func() { err = errors.Join(err, restore()) }()
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
	for _, _, n := held(r.exe, clone); time.Now().Before(deadline); {
		time.Sleep(readerStill)
		_, _, m := held(r.exe, clone)
		if m == n {
			break
		}
		n = m
	}
	return true, nil
}

func hideTransient(clone string, nodeName func(path string) (string, error)) (restore func() error) {
	entries, _ := os.ReadDir(inputDir)
	var aside []string
	for _, e := range entries {
		path := filepath.Join(inputDir, e.Name())
		if path == clone {
			continue
		}
		if name, err := nodeName(path); err != nil || name != transientNode {
			continue
		}
		to := filepath.Join(filepath.Dir(inputDir), asidePrefix+e.Name())
		if os.Rename(path, to) == nil {
			aside = append(aside, to)
		}
	}
	return func() error {
		var errs []error
		for _, to := range aside {
			if err := restoreNode(to); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

func restoreNode(aside string) error {
	path := filepath.Join(inputDir, strings.TrimPrefix(filepath.Base(aside), asidePrefix))
	if f, err := os.Open(aside); err == nil {
		f.Close()
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(aside, path); err != nil {
				return fmt.Errorf("putting %s back: %w", path, err)
			}
			return nil
		}
	}
	if err := os.Remove(aside); err != nil {
		return fmt.Errorf("putting %s back: %w", path, err)
	}
	return nil
}

func settled(exe, clone string) bool {
	live, dead, _ := held(exe, clone)
	return live && !dead
}

func held(exe, clone string) (live, dead bool, inputs int) {
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
			if strings.HasPrefix(target, inputDir+"/") {
				inputs++
			}
			live = live || target == clone
			dead = dead || strings.HasPrefix(target, inputDir+"/") && strings.HasSuffix(target, " (deleted)")
		}
	}
	return live, dead, inputs
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
