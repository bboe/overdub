package audio

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

const idleStatus = `state: XRUN
owner_pid   : 629
trigger_time: 51391.544955203
tstamp      : 52477.314108112
delay       : 0
avail       : 2352
avail_max   : 0
-----
hw_ptr      : 487728
appl_ptr    : 488448
`

func status(t *testing.T, text string) pcmStatus {
	t.Helper()
	st, err := parseStatus(bufio.NewScanner(strings.NewReader(text)))
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	return st
}

func TestTheStatusFileIsReadAsTheDriverWritesIt(t *testing.T) {
	st := status(t, idleStatus)
	if st.State != "XRUN" {
		t.Errorf("state came back as %q; it is the one line the driver writes with no"+
			" padding before the colon", st.State)
	}
	if st.Delay != 0 {
		t.Errorf("delay came back as %d, want 0", st.Delay)
	}
}

func TestAnOutputThatIsNotRunningIsNotMistakenForOne(t *testing.T) {
	if status(t, idleStatus).running() {
		t.Error("an output in XRUN was read as running, and its delay of 0 would be" +
			" taken for a queue that had drained rather than one that never filled")
	}
	if !status(t, strings.Replace(idleStatus, "XRUN", "RUNNING", 1)).running() {
		t.Error("a running output was not read as one")
	}
}

func TestAStatusNamingNoStateIsRefused(t *testing.T) {
	without := strings.Replace(idleStatus, "state: XRUN\n", "", 1)
	if _, err := parseStatus(bufio.NewScanner(strings.NewReader(without))); err == nil {
		t.Error("a status file with no state line was read as one, so a closed output" +
			" reports whatever delay was left in the file")
	}
}

func garble(t *testing.T, field string) string {
	t.Helper()
	lines := strings.Split(idleStatus, "\n")
	for i, line := range lines {
		if name, _, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == field {
			lines[i] = name + ": x"
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("the fixture has no %s line, so nothing was garbled", field)
	return ""
}

func TestAStatusFieldThatIsNotANumberIsRefused(t *testing.T) {
	for _, field := range []string{"delay"} {
		text := garble(t, field)
		if _, err := parseStatus(bufio.NewScanner(strings.NewReader(text))); err == nil {
			t.Errorf("%s was read as a number when it was not, so it counts as zero and"+
				" the position it feeds is silently wrong", field)
		}
	}
}

func TestAMissingStatusFileIsReported(t *testing.T) {
	if _, err := readStatus("/proc/asound/card0/pcm99p/sub0/status"); err == nil {
		t.Error("a status file that is not there was read without complaint")
	}
}

func running(delay int64) pcmStatus { return pcmStatus{State: "RUNNING", Delay: delay} }

func TestFramesAtTheDACAreWhatThePlayerTookLessWhatTheHALHolds(t *testing.T) {
	at := time.Now()
	pt, err := point(1000, running(2000), at)
	if err != nil {
		t.Fatalf("point: %v", err)
	}
	if pt.Frames != 46000 {
		t.Errorf("a second of audio with 2000 frames still held came back as %d, want"+
			" 48000 - 2000", pt.Frames)
	}
	if !pt.At.Equal(at) {
		t.Error("the reading came back stamped at a different moment than it was taken")
	}
}

func TestAudioQueuedButNotYetHeardReadsNegative(t *testing.T) {
	pt, err := point(10, running(3000), time.Now())
	if err != nil {
		t.Fatalf("point: %v", err)
	}
	if pt.Frames >= 0 {
		t.Errorf("frames came back as %d; audio that is queued and has not reached the"+
			" DAC is how far ahead the writer is, and clamping it reads as here now",
			pt.Frames)
	}
}

func TestAPlayerThatWillNotSayWhereItIsIsRefused(t *testing.T) {
	if _, err := point(-1, running(0), time.Now()); err == nil {
		t.Error("a player that reported no position was believed, and the frames it" +
			" implies are counted backwards from zero")
	}
}

func TestAPositionIsRefusedWhenTheOutputIsNotRunning(t *testing.T) {
	if _, err := point(1000, pcmStatus{State: "XRUN"}, time.Now()); err == nil {
		t.Error("a delay of zero from a stopped output was taken for a drained queue," +
			" which places our frames about 57 ms later than they are")
	}
}

func TestAStatusNamingNoDelayIsRefused(t *testing.T) {
	without := strings.Replace(idleStatus, "delay       : 0\n", "", 1)
	if without == idleStatus {
		t.Fatal("the fixture's delay line was not removed, so nothing here is tested")
	}
	if _, err := parseStatus(bufio.NewScanner(strings.NewReader(without))); err == nil {
		t.Error("a status with no delay line read as a drained queue, which overstates" +
			" the position by the whole HAL buffer of 58 to 64 ms")
	}
}
