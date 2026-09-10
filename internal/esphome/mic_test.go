package esphome

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMic struct {
	mu       sync.Mutex
	muted    bool
	known    bool
	presses  int
	pressErr error
}

func wireFakeMic(s *Server, muted bool) *fakeMic {
	f := &fakeMic{muted: muted, known: true}
	s.micMute = func() (bool, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.muted, f.known
	}
	s.micPress = func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.presses++
		if f.pressErr != nil {
			return f.pressErr
		}
		f.muted = !f.muted
		return nil
	}
	return f
}

func (f *fakeMic) state() (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.muted, f.presses
}

func waitMicIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.micWorking || s.micHasPending
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the microphone worker never went idle")
}

func setMic(s *Server, want bool) {
	s.mu.Lock()
	s.setMicLocked(&conn{sock: fakeAddr{}}, want)
	s.mu.Unlock()
}

func TestTheSwitchMutesAndUnmutesTheMicrophone(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	f := wireFakeMic(s, false)

	setMic(s, true)
	waitMicIdle(t, s)
	if muted, presses := f.state(); !muted || presses != 1 {
		t.Errorf("muting gave muted=%v after %d presses, want true after 1", muted, presses)
	}

	setMic(s, false)
	waitMicIdle(t, s)
	if muted, presses := f.state(); muted || presses != 2 {
		t.Errorf("unmuting gave muted=%v after %d presses, want false after 2", muted, presses)
	}
}

func TestAMicrophoneAlreadyWhereItWasAskedIsNotPressed(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	f := wireFakeMic(s, true)

	setMic(s, true)
	waitMicIdle(t, s)
	if muted, presses := f.state(); !muted || presses != 0 {
		t.Errorf("a microphone already muted was pressed %d times, and is now muted=%v", presses, muted)
	}
}

func TestAnUnreadableMicrophoneIsNotPressed(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	f := wireFakeMic(s, true)
	f.mu.Lock()
	f.known = false
	f.mu.Unlock()

	setMic(s, false)
	waitMicIdle(t, s)
	if _, presses := f.state(); presses != 0 {
		t.Errorf("a microphone that could not be read was pressed %d times", presses)
	}
	waitForLog(t, &out, "could not be read")
}

func TestASwitchFlippedToWhereTheLastApplyLandedCostsNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	stubSensors(s)
	f := wireFakeMic(s, false)

	setMic(s, true)
	waitMicIdle(t, s)

	reads := 0
	s.micMute = func() (bool, bool) {
		reads++
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.muted, f.known
	}
	_, before := f.state()

	setMic(s, true)
	waitMicIdle(t, s)
	if reads != 0 {
		t.Errorf("a switch flipped to where the last apply landed read the device %d times", reads)
	}
	if _, presses := f.state(); presses != before {
		t.Errorf("it pressed the key %d times, want none beyond the first apply", presses-before)
	}
}

func TestAPressThatFailedIsNotReportedAsAMute(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	f := wireFakeMic(s, false)
	f.mu.Lock()
	f.pressErr = errors.New("uinput: write: bad file descriptor")
	f.mu.Unlock()

	setMic(s, true)
	waitMicIdle(t, s)
	if muted, _ := f.state(); muted {
		t.Error("a press that failed muted the microphone anyway")
	}
	waitForLog(t, &out, "bad file descriptor")
	if strings.Contains(out.String(), "microphone: muted") {
		t.Errorf("the log reported a mute that never happened: %q", out.String())
	}
}

func TestASwitchCommandForAnotherKeyIsIgnored(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	f := wireFakeMic(s, false)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keySound, true)); err != nil {
		t.Fatalf("a switch command for another key was an error: %v", err)
	}
	waitMicIdle(t, s)
	if _, presses := f.state(); presses != 0 {
		t.Errorf("a command naming the speaker pressed the mute key %d times", presses)
	}

	if err := s.handle(c, msgSwitchCommand, switchCommand(s.keyMicMute, true)); err != nil {
		t.Fatalf("a switch command for the microphone was an error: %v", err)
	}
	waitMicIdle(t, s)
	if muted, presses := f.state(); !muted || presses != 1 {
		t.Errorf("the microphone is muted=%v after %d presses, want true after 1", muted, presses)
	}
}

func switchCommand(key uint32, on bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(2, on)
	return p.b
}

func TestTheMicrophoneWakesThePollThatReadsIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	wireFakeMic(s, false)

	setMic(s, true)
	waitMicIdle(t, s)

	if len(s.liveWake) != 1 {
		t.Error("the live poll was not woken, and it is the one that reads the microphone")
	}
	if len(s.sensorWake) != 0 {
		t.Error("the sensor poll was woken, and it never reads the microphone: " +
			"that turn forks iptables for the adb rule before it reads anything")
	}
}

func TestChangingYourMindBeforeTheStateIsPublishedIsNotDropped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	stubSensors(s)
	f := wireFakeMic(s, false)

	s.publish("sensors", s.readLive())

	setMic(s, true)
	waitMicIdle(t, s)
	setMic(s, false)
	waitMicIdle(t, s)

	if muted, presses := f.state(); muted || presses != 2 {
		t.Errorf("after mute then unmute the microphone is muted=%v after %d presses, "+
			"want live after 2", muted, presses)
	}
}

func TestAMicrophoneMovedAtTheDotCanBeAskedForAgain(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.micSettle = time.Millisecond
	stubSensors(s)
	f := wireFakeMic(s, false)

	setMic(s, true)
	waitMicIdle(t, s)

	f.mu.Lock()
	f.muted = false
	f.mu.Unlock()
	s.publish("sensors", s.readLive())

	setMic(s, true)
	waitMicIdle(t, s)
	if muted, presses := f.state(); !muted || presses != 2 {
		t.Errorf("the microphone is muted=%v after %d presses, want muted after 2", muted, presses)
	}
}
