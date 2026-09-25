package audio

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
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

func TestWhatIsAheadIsOurQueueAndTheHALTogether(t *testing.T) {
	at := time.Now()
	pt, err := point(3840, running(3056), at, ChimeRate)
	if err != nil {
		t.Fatalf("point: %v", err)
	}
	if pt.Ahead != 6896 {
		t.Errorf("a full queue over a 3,056-frame HAL buffer came back as %d, want the"+
			" 6,896 the two hold between them", pt.Ahead)
	}
	if !pt.At.Equal(at) {
		t.Error("the reading came back stamped at a different moment than it was taken")
	}
}

func TestTheHALsDelayIsCountedInThePlayersFrames(t *testing.T) {
	pt, err := point(3840, running(3072), time.Now(), BluetoothRate)
	if err != nil {
		t.Fatalf("point: %v", err)
	}
	if pt.Ahead != 3840+2822 {
		t.Errorf("a full queue at %d Hz over a HAL holding 3,072 frames at %d Hz came back"+
			" as %d, want %d: the HAL's 64 ms is 2,822 of the player's frames",
			BluetoothRate, ChimeRate, pt.Ahead, 3840+2822)
	}
}

func TestThePipelineCeilingIsOneSecondAtThePlayersRate(t *testing.T) {
	if _, err := point(0, running(ChimeRate), time.Now(), BluetoothRate); err != nil {
		t.Errorf("a pipeline of exactly 1 second at %d Hz was refused: %v", BluetoothRate, err)
	}
	if _, err := point(1, running(ChimeRate), time.Now(), BluetoothRate); err == nil {
		t.Errorf("a pipeline 1 frame past 1 second at %d Hz was believed", BluetoothRate)
	}
}

func TestAPlayerThatWillNotSayWhatItHoldsIsRefused(t *testing.T) {
	if _, err := point(-1, running(0), time.Now(), ChimeRate); err == nil {
		t.Error("a player that would not report its queue was believed, and a stream is" +
			" then placed against a pipeline of nothing but the HAL")
	}
}

func TestAReadingIsRefusedWhenTheOutputIsNotRunning(t *testing.T) {
	if _, err := point(3840, pcmStatus{State: "XRUN"}, time.Now(), ChimeRate); err == nil {
		t.Error("a delay of zero from a stopped output was taken for a drained queue," +
			" which places our frames about 57 ms early")
	}
}

func TestAPipelineDeeperThanTheQueueAndAnyBufferIsRefused(t *testing.T) {
	if _, err := point(3840, running(aheadCeiling), time.Now(), ChimeRate); err == nil {
		t.Errorf("a pipeline past %d frames was believed. Nothing here can hold that"+
			" much: the queue is eight blocks and the HAL buffer measured 58 to 64 ms,"+
			" so a bigger number is a broken instrument -- and believing one places"+
			" every frame that far out, consistently enough that the slip check never"+
			" fires and the whole stream is dropped as late with nothing to say why",
			aheadCeiling)
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

func TestANegativeDelayIsRefusedWithNothingInOurQueueToHideIt(t *testing.T) {
	if _, err := point(0, running(-3000), time.Now(), ChimeRate); err == nil {
		t.Error("a negative delay was believed with an empty queue, which places the" +
			" stream that far late -- consistently, which is the one shape the slip" +
			" check cannot see")
	}
}

func TestANegativeDelayIsRefusedEvenWhenOurOwnQueueCoversItUp(t *testing.T) {
	if _, err := point(3840, running(-3000), time.Now(), ChimeRate); err == nil {
		t.Error("a full queue over a delay of -3,000 was believed, and it reads as a" +
			" shallow pipeline rather than as a refusal: the stream then anchors 62.5 ms" +
			" short and places every frame that far late, consistently, which is the one" +
			" shape the slip check cannot see")
	}
}

func TestADelaySoLargeThatTheSumWrapsIsRefused(t *testing.T) {
	if _, err := point(3840, running(math.MaxInt64), time.Now(), ChimeRate); err == nil {
		t.Error("a delay of the largest int64 was believed: the sum wraps negative, which" +
			" is under the ceiling rather than over it, and the stream is then placed" +
			" against a pipeline it reads as being behind the speaker")
	}
}

const socketsA2DP = `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 675213 @/data/misc/bluedroid/.a2dp_data
0000000000000000: 00000002 00000000 00010000 0001 01 627612 @/data/misc/bluedroid/.a2dp_ctrl
0000000000000000: 00000003 00000000 00000000 0001 03 688233 @/data/misc/bluedroid/.a2dp_ctrl
0000000000000000: 00000003 00000000 00000000 0001 03 675215 @/data/misc/bluedroid/.a2dp_data
0000000000000000: 00000003 00000000 00000000 0001 03 12345
`

const socketsIdle = `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 627612 @/data/misc/bluedroid/.a2dp_ctrl
0000000000000000: 00000003 00000000 00000000 0001 03 12345
`

func writeFile(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOnlyAConnectedA2DPDataSocketMeansBluetooth(t *testing.T) {
	for _, tt := range []struct {
		name  string
		table string
		want  bool
	}{
		{"a speaker taking audio", socketsA2DP, true},
		{"nothing connected", socketsIdle, false},
		{"the data socket only listening",
			"x: 00000002 00000000 00010000 0001 01 675213 @/data/misc/bluedroid/.a2dp_data\n",
			false},
		{"only the control socket connected",
			"x: 00000003 00000000 00000000 0001 03 688233 @/data/misc/bluedroid/.a2dp_ctrl\n",
			false},
		{"a path that only ends the same way",
			"x: 00000003 00000000 00000000 0001 03 1 @/data/misc/bluedroid/.a2dp_data2\n",
			false},
		{"a path with more after a space",
			"x: 00000003 00000000 00000000 0001 03 1 @/data/misc/bluedroid/.a2dp_data x\n",
			false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := overBluetooth(writeFile(t, "unix", tt.table)); got != tt.want {
				t.Errorf("overBluetooth = %v, want %v", got, tt.want)
			}
		})
	}
	if overBluetooth(filepath.Join(t.TempDir(), "gone")) {
		t.Error("a socket table that could not be read was taken for a speaker")
	}
}

func TestTheOutputRateIsTheRateOfTheOutputInUse(t *testing.T) {
	if got := outputRate(writeFile(t, "unix", socketsA2DP)); got != BluetoothRate {
		t.Errorf("with a speaker taking audio the output rate is %d, want %d", got,
			BluetoothRate)
	}
	if got := outputRate(writeFile(t, "unix", socketsIdle)); got != ChimeRate {
		t.Errorf("with no speaker connected the output rate is %d, want %d", got, ChimeRate)
	}
}

func TestBluetoothTakesTheA2DPDelayWhateverThePCMSays(t *testing.T) {
	sockets := writeFile(t, "unix", socketsA2DP)
	for _, pcm := range []string{
		writeFile(t, "idle", idleStatus),
		writeFile(t, "busy", strings.Replace(idleStatus, "XRUN", "RUNNING", 1)),
		filepath.Join(t.TempDir(), "gone"),
	} {
		want := pcmStatus{State: "RUNNING", Delay: a2dpDelay, bursty: true}
		st, err := outputStatus(sockets, pcm)
		if err != nil || st != want {
			t.Errorf("over Bluetooth the output read %+v, %v; want %+v", st, err, want)
		}
	}
}

func TestAReadingSaysWhetherItsOutputTakesAudioInBursts(t *testing.T) {
	pt, err := point(3840, pcmStatus{State: "RUNNING", Delay: a2dpDelay, bursty: true}, time.Now(), ChimeRate)
	if err != nil || !pt.Bursty {
		t.Errorf("a Bluetooth reading came back as %+v, %v; want it marked bursty", pt, err)
	}
	if pt, _ := point(3840, running(3056), time.Now(), ChimeRate); pt.Bursty {
		t.Error("a reading of the speaker's PCM was marked bursty")
	}
}

func TestWithoutBluetoothThePCMStatusDecides(t *testing.T) {
	sockets := writeFile(t, "unix", socketsIdle)
	st, err := outputStatus(sockets, writeFile(t, "idle", idleStatus))
	if err != nil || st.running() {
		t.Errorf("the speaker's idle PCM read %+v, %v; want it not running", st, err)
	}
	if _, err := outputStatus(sockets, filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Error("a missing status file was not reported with no speaker connected")
	}
}
