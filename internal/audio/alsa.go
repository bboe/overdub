package audio

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	statusGlob   = "/proc/asound/card*/pcm*p/sub0/status"
	mixerExe     = "/system/bin/mixer"
	procRoot     = "/proc"
	outputRescan = 100 * time.Millisecond
	ownerRescan  = 5 * time.Second
)

const (
	socketsPath    = "/proc/net/unix"
	a2dpDataSocket = "@/data/misc/bluedroid/.a2dp_data"
	socketLinked   = "03"
	a2dpDelay      = 407 * ChimeRate / 1000
	BluetoothRate  = 44100
)

type pcmStatus struct {
	State  string
	Delay  int64
	Owner  int
	bursty bool
}

func (s pcmStatus) running() bool { return s.State == "RUNNING" }

func parseStatus(r *bufio.Scanner) (pcmStatus, error) {
	var st pcmStatus
	var sawState, sawDelay bool
	for r.Scan() {
		name, value, ok := strings.Cut(r.Text(), ":")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch name {
		case "state":
			st.State, sawState = value, true
			continue
		case "owner_pid":
			if n, err := strconv.Atoi(value); err == nil {
				st.Owner = n
			}
			continue
		case "delay":
		default:
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return pcmStatus{}, fmt.Errorf("audio: the output reports %s = %q", name, value)
		}
		st.Delay, sawDelay = n, true
	}
	if err := r.Err(); err != nil {
		return pcmStatus{}, err
	}
	if !sawState {
		return pcmStatus{}, errors.New("audio: the output names no state")
	}
	if !sawDelay {
		return pcmStatus{}, errors.New("audio: the output names no delay")
	}
	return st, nil
}

func readStatus(path string) (pcmStatus, error) {
	f, err := os.Open(path)
	if err != nil {
		return pcmStatus{}, err
	}
	defer f.Close()
	return parseStatus(bufio.NewScanner(f))
}

type outputFinder struct {
	glob, exe, proc string

	path      string
	owner     int
	next      time.Time
	ownerNext time.Time
}

func newOutputFinder() *outputFinder {
	return &outputFinder{glob: statusGlob, exe: mixerExe, proc: procRoot}
}

func (f *outputFinder) find(now time.Time) string {
	if f.path != "" {
		if st, err := readStatus(f.path); err == nil && st.running() {
			return f.path
		}
	}
	if now.Before(f.next) {
		return f.path
	}
	f.next = now.Add(outputRescan)
	if (f.owner == 0 || !runs(f.proc, f.owner, f.exe)) && !now.Before(f.ownerNext) {
		f.ownerNext = now.Add(ownerRescan)
		f.owner = processID(f.proc, f.exe)
	}
	if f.owner == 0 {
		return f.path
	}
	paths, _ := filepath.Glob(f.glob)
	for _, path := range paths {
		if st, err := readStatus(path); err == nil && st.running() && f.threadOf(f.owner, st.Owner) {
			f.path = path
			break
		}
	}
	return f.path
}

func (f *outputFinder) threadOf(pid, tid int) bool {
	_, err := os.Stat(filepath.Join(f.proc, strconv.Itoa(pid), "task", strconv.Itoa(tid)))
	return tid > 0 && err == nil
}

func runs(proc string, pid int, exe string) bool {
	argv, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cmdline"))
	first, _, _ := strings.Cut(string(argv), "\x00")
	return err == nil && first == exe
}

func processID(proc, exe string) int {
	dirs, _ := filepath.Glob(filepath.Join(proc, "[0-9]*"))
	for _, dir := range dirs {
		if pid, err := strconv.Atoi(filepath.Base(dir)); err == nil && runs(proc, pid, exe) {
			return pid
		}
	}
	return 0
}

func overBluetooth(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewScanner(f)
	for r.Scan() {
		fields := strings.Fields(r.Text())
		if len(fields) == 8 && fields[7] == a2dpDataSocket && fields[5] == socketLinked {
			return true
		}
	}
	return false
}

func OutputRate() int { return outputRate(socketsPath) }

func outputRate(sockets string) int {
	if overBluetooth(sockets) {
		return BluetoothRate
	}
	return ChimeRate
}

func outputStatus(sockets string, status func() string) (pcmStatus, error) {
	if overBluetooth(sockets) {
		return pcmStatus{State: "RUNNING", Delay: a2dpDelay, bursty: true}, nil
	}
	path := status()
	if path == "" {
		return pcmStatus{}, errors.New("audio: no playback device the mixer owns is running")
	}
	return readStatus(path)
}

const aheadCeiling = ChimeRate

type Point struct {
	Ahead  int64
	At     time.Time
	Bursty bool
}

func point(pending int64, st pcmStatus, at time.Time, rate int) (Point, error) {
	if pending < 0 {
		return Point{}, errors.New("audio: the player would not say how much of what it" +
			" was written it still holds")
	}
	if !st.running() {
		return Point{}, fmt.Errorf("audio: the output is %s rather than running, so what"+
			" it still holds is not a delay", st.State)
	}
	if st.Delay < 0 {
		return Point{}, fmt.Errorf("audio: the output says it is %d frames behind what has"+
			" been written to it, and a delay below zero is not a measurement", st.Delay)
	}
	if st.Delay <= aheadCeiling {
		ahead := pending + st.Delay*int64(rate)/ChimeRate
		if ahead >= 0 && ahead <= int64(rate)*aheadCeiling/ChimeRate {
			return Point{Ahead: ahead, At: at, Bursty: st.bursty}, nil
		}
	}
	return Point{}, fmt.Errorf("audio: the player claims to hold %d frames at %d Hz and the"+
		" output %d at %d Hz, and a pipeline outside 0 to %s is not a measurement",
		pending, rate, st.Delay, ChimeRate, frameTime(ChimeRate, aheadCeiling))
}
