package audio

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const statusPath = "/proc/asound/card0/pcm23p/sub0/status"

const (
	socketsPath    = "/proc/net/unix"
	a2dpDataSocket = "@/data/misc/bluedroid/.a2dp_data"
	socketLinked   = "03"
	a2dpDelay      = 407 * ChimeRate / 1000
)

type pcmStatus struct {
	State  string
	Delay  int64
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
		case "delay":
		default:
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return pcmStatus{}, fmt.Errorf("audio: %s reports %s = %q", statusPath, name, value)
		}
		st.Delay, sawDelay = n, true
	}
	if err := r.Err(); err != nil {
		return pcmStatus{}, err
	}
	if !sawState {
		return pcmStatus{}, fmt.Errorf("audio: %s names no state", statusPath)
	}
	if !sawDelay {
		return pcmStatus{}, fmt.Errorf("audio: %s names no delay", statusPath)
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

func outputStatus(sockets, status string) (pcmStatus, error) {
	if overBluetooth(sockets) {
		return pcmStatus{State: "RUNNING", Delay: a2dpDelay, bursty: true}, nil
	}
	return readStatus(status)
}

const aheadCeiling = ChimeRate

type Point struct {
	Ahead  int64
	At     time.Time
	Bursty bool
}

func point(pending int64, st pcmStatus, at time.Time) (Point, error) {
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
	if ahead := pending + st.Delay; ahead >= 0 && ahead <= aheadCeiling {
		return Point{Ahead: ahead, At: at, Bursty: st.bursty}, nil
	}
	return Point{}, fmt.Errorf("audio: the player and the output claim to hold %d frames"+
		" between them, and a pipeline outside 0 to %d is not a measurement",
		pending+st.Delay, aheadCeiling)
}
