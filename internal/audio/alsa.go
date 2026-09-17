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

type pcmStatus struct {
	State string
	Delay int64
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

type Point struct {
	Frames int64
	At     time.Time
}

func point(ms int64, st pcmStatus, at time.Time) (Point, error) {
	if ms < 0 {
		return Point{}, errors.New("audio: the player would not report its position")
	}
	if !st.running() {
		return Point{}, fmt.Errorf("audio: the output is %s rather than running, so what"+
			" it still holds is not a delay", st.State)
	}
	return Point{Frames: ms*ChimeRate/1000 - st.Delay, At: at}, nil
}
