package esphome

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

type fakeADB struct {
	mu     sync.Mutex
	asked  []device.ADBMode
	live   device.ADBMode
	became func(device.ADBMode) device.ADBMode
	done   chan struct{}
	gate   chan struct{}

	denies int
	setErr error
}

func wireFakeADB(s *Server, secureOK bool) *fakeADB {
	f := &fakeADB{done: make(chan struct{}, 16)}
	s.adbSecureOK = func() bool { return secureOK }
	s.adbSet = func(m device.ADBMode) error {
		f.mu.Lock()
		gate := f.gate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		f.asked = append(f.asked, m)
		f.live = m
		if f.became != nil {
			f.live = f.became(m)
		}
		err := f.setErr
		f.mu.Unlock()
		return err
	}
	s.adbMode = func() (device.ADBMode, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.live, true
	}
	s.adbDeny = func() error {
		f.mu.Lock()
		f.denies++
		f.mu.Unlock()
		return nil
	}
	s.adbHold = func() error {
		select {
		case f.done <- struct{}{}:
		default:
		}
		return nil
	}
	return f
}

func (f *fakeADB) askedFor() []device.ADBMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]device.ADBMode(nil), f.asked...)
}

func (f *fakeADB) waitAsked(t *testing.T, n int) []device.ADBMode {
	t.Helper()
	var got []device.ADBMode
	waitFor(t, fmt.Sprintf("%d calls to the device", n), func() bool {
		got = f.askedFor()
		return len(got) >= n
	})
	return got
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForLog(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("the log to say %q", want), func() bool {
		return strings.Contains(out.String(), want)
	})
}

func TestEachOfferedADBModeReachesTheDevice(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	for i, want := range []device.ADBMode{device.ADBInsecure, device.ADBSecure, device.ADBOff} {
		s.mu.Lock()
		s.setADBLocked(&conn{sock: fakeAddr{}}, want.String())
		s.mu.Unlock()
		if got := f.waitAsked(t, i+1); got[i] != want {
			t.Errorf("the device was asked for %v, want %v", got[i], want)
		}
	}
}

func TestAnADBModeThatWasNeverOfferedIsRefused(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	for _, choice := range []string{"", "off", "OFF", "on", "true", "Wide Open"} {
		s.mu.Lock()
		s.setADBLocked(&conn{sock: fakeAddr{}}, choice)
		s.mu.Unlock()
	}
	waitADBIdle(t, s)
	if got := f.askedFor(); len(got) != 0 {
		t.Errorf("a mode nobody offered reached the device: %v", got)
	}
}

func TestSecureIsRefusedWithoutAKeyToAuthenticateAgainst(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, false)
	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBSecure.String())
	s.mu.Unlock()
	waitADBIdle(t, s)
	if got := f.askedFor(); len(got) != 0 {
		t.Errorf("Secure reached the device with no key installed: %v", got)
	}
}

func TestSecureThatCameUpInsecureIsClosedInstead(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	f.mu.Lock()
	f.became = func(m device.ADBMode) device.ADBMode {
		if m == device.ADBSecure {
			return device.ADBInsecure
		}
		return m
	}
	f.mu.Unlock()

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBSecure.String())
	s.mu.Unlock()
	if got := f.waitAsked(t, 2); got[1] != device.ADBOff {
		t.Errorf("the second call asked for %v, want Off: a port that authenticates nobody is worse than a closed one", got[1])
	}
	waitForLog(t, &out, "closed instead")
}

func TestRapidMovesCollapseToTheLastOne(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	s.mu.Lock()
	for _, choice := range []device.ADBMode{device.ADBInsecure, device.ADBSecure, device.ADBOff} {
		s.setADBLocked(&conn{sock: fakeAddr{}}, choice.String())
	}
	s.mu.Unlock()
	if got := f.waitAsked(t, 1); len(got) != 1 || got[0] != device.ADBOff {
		t.Errorf("the device was asked for %v, want only [Off]: every position in between is a port nobody wanted", got)
	}
}

func TestTheSensorPollPutsTheRuleBack(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	stubSensors(s)
	s.adbMode = func() (device.ADBMode, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.live, true
	}

	go s.PollSensors(MinSensorTick)
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never asserted the rule at startup")
	}

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)

	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Error("the rule was never re-asserted for an open port, so netd's next rebuild closes it for good")
	}
}

func TestARepeatedADBCommandDoesNotRestartAdbd(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	stubSensors(s)
	s.adbMode = func() (device.ADBMode, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.live, true
	}

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)

	waitADBIdle(t, s)
	s.publish("sensors", s.readTicked())

	for range 3 {
		s.mu.Lock()
		s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
		s.mu.Unlock()
	}
	waitADBIdle(t, s)
	if got := f.askedFor(); len(got) != 1 {
		t.Errorf("the device was asked %d times for a position it was already in: %v", len(got), got)
	}
}

func TestAnADBCommandRepeatingTheOneInFlightIsDropped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()

	waitFor(t, "the worker to reach the apply", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.adbWorking && !s.adbHasPending
	})

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	queued := s.adbHasPending
	s.mu.Unlock()
	if queued {
		t.Error("a command naming the mode already being applied was queued behind it")
	}

	f.mu.Lock()
	close(f.gate)
	f.gate = nil
	f.mu.Unlock()

	f.waitAsked(t, 1)
	waitADBIdle(t, s)
	if got := f.askedFor(); len(got) != 1 {
		t.Errorf("adbd was restarted %d times for one position: %v", len(got), got)
	}
}

func waitADBIdle(t *testing.T, s *Server) {
	t.Helper()
	waitFor(t, "the adb worker to go idle", func() bool { return !s.adbBusy() })
}

func TestClosingTheportDeletesTheRuleAgainAfterTheSettle(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBOff.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)

	waitFor(t, "the rule to be deleted again after the port was closed", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.denies > 0
	})
}

func TestAPositionThatFailedCanBeAskedForAgain(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	stubSensors(s)
	f.mu.Lock()
	f.setErr = errors.New("iptables: could not get the xtables lock")
	f.mu.Unlock()

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBOff.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)
	waitADBIdle(t, s)
	s.publish("sensors", s.readTicked())

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBOff.String())
	s.mu.Unlock()
	if got := f.waitAsked(t, 2); len(got) != 2 {
		t.Errorf("the device was asked %d times; a position that failed must stay askable", len(got))
	}
}

func TestThePollDoesNotReassertWhileAPositionIsBeingApplied(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	stubSensors(s)

	go s.PollSensors(MinSensorTick)
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never asserted the rule at startup")
	}

	s.mu.Lock()
	s.adbWorking = true
	s.mu.Unlock()
	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
	select {
	case <-f.done:
		t.Error("the rule was re-asserted while a position was still being applied")
	case <-time.After(3 * s.wakeGap):
	}

	s.mu.Lock()
	s.adbWorking = false
	s.mu.Unlock()
	select {
	case s.sensorWake <- struct{}{}:
	default:
	}
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Error("the poll stopped re-asserting the rule once the worker was done")
	}
}

func TestAModeThatCouldNotBeReadIsNotPublished(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.adbMode = func() (device.ADBMode, bool) { return device.ADBOff, false }

	for _, r := range s.readTicked() {
		if r.key == s.keyADB {
			t.Errorf("an unreadable mode went out as %q", r.text)
		}
	}

	s.adbMode = func() (device.ADBMode, bool) { return device.ADBInsecure, true }
	found := false
	for _, r := range s.readTicked() {
		if r.key == s.keyADB {
			found = true
		}
	}
	if !found {
		t.Error("a mode that could be read was not published either")
	}
}

func TestARepeatBeforeTheStateIsPublishedDoesNotRestartAdbd(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	stubSensors(s)
	s.adbMode = func() (device.ADBMode, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.live, true
	}

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)
	waitADBIdle(t, s)

	for range 3 {
		s.mu.Lock()
		s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
		s.mu.Unlock()
	}
	waitADBIdle(t, s)
	if got := f.askedFor(); len(got) != 1 {
		t.Errorf("the device was asked %d times for the position it had just reached: %v", len(got), got)
	}
}

func TestAModeTheDeviceLeftCanBeAskedForAgain(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	stubSensors(s)
	s.adbMode = func() (device.ADBMode, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.live, true
	}

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)
	waitADBIdle(t, s)

	f.mu.Lock()
	f.live = device.ADBOff
	f.mu.Unlock()
	s.publish("sensors", s.readTicked())

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()
	if got := f.waitAsked(t, 2); got[1] != device.ADBInsecure {
		t.Errorf("the second call asked for %v, want Insecure", got[1])
	}
}

func TestTheRevertLineDoesNotInventAMode(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	f.mu.Lock()
	f.became = func(m device.ADBMode) device.ADBMode {
		if m == device.ADBSecure {
			return device.ADBInsecure
		}
		return m
	}
	f.mu.Unlock()

	var reads int
	s.adbMode = func() (device.ADBMode, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reads++
		if reads > 1 {
			return device.ADBOff, false
		}
		return f.live, true
	}

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBSecure.String())
	s.mu.Unlock()
	f.waitAsked(t, 2)
	waitForLog(t, &out, "closed instead, and the device could not be read")
	if strings.Contains(out.String(), "closed instead; device is") {
		t.Errorf("the log named a mode nobody read: %q", out.String())
	}
}
