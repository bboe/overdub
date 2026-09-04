package esphome

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

// A fake adbd: what it was asked for, and what the device turns out to be
// afterwards, which are two different things and the whole reason setADBMode
// reads the device back instead of trusting its own call.
type fakeADB struct {
	mu     sync.Mutex
	asked  []device.ADBMode
	live   device.ADBMode
	became func(device.ADBMode) device.ADBMode
	done   chan struct{}
	// Closed to let a blocked apply finish. adbd takes seconds to restart, and
	// the window this opens is the one a second command actually lands in.
	gate chan struct{}

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

// The worker runs off the connection's goroutine, so a test has to wait for it
// rather than read straight after the command. Waiting on what was asked rather
// than on what the device now is: Off is the zero value, so a wait for that
// state is satisfied before anything has happened at all.
func (f *fakeADB) waitAsked(t *testing.T, n int) []device.ADBMode {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.askedFor(); len(got) >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the device was asked for %v, want %d calls", f.askedFor(), n)
	return nil
}

// The lines come after the call that provoked them, so a test that read the log
// the moment the device moved would be reading it too early.
func waitForLog(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("nothing in the log says %q: %q", want, out.String())
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

// The listing is what Home Assistant was told it could pick from, so anything
// else is a peer inventing an option rather than somebody choosing one.
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
	time.Sleep(20 * time.Millisecond)
	if got := f.askedFor(); len(got) != 0 {
		t.Errorf("a mode nobody offered reached the device: %v", got)
	}
}

// Secure without a key is not a weaker Secure, it is Insecure: adbd comes up
// listening and authenticates nobody. Refusing it is the same rule as refusing
// a word that was never listed, since the listing leaves it out for this reason.
func TestSecureIsRefusedWithoutAKeyToAuthenticateAgainst(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, false)
	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBSecure.String())
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if got := f.askedFor(); len(got) != 0 {
		t.Errorf("Secure reached the device with no key installed: %v", got)
	}
}

// The gap between setting the property and adbd coming up is where this can go
// wrong in the one direction that matters: asked to authenticate and listening
// without doing so. The port is closed rather than left open and reported.
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

// A dropdown dragged through three positions is three commands and one device
// that takes seconds to answer. Landing on the last is the point; visiting the
// ones in between is a port opened because somebody's finger passed over it.
func TestRapidMovesCollapseToTheLastOne(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.adbSettle = time.Millisecond
	f := wireFakeADB(s, true)
	// Held so the worker cannot start until every command is in, which is the
	// case this collapses: the lock is what handle holds across each one.
	s.mu.Lock()
	for _, choice := range []device.ADBMode{device.ADBInsecure, device.ADBSecure, device.ADBOff} {
		s.setADBLocked(&conn{sock: fakeAddr{}}, choice.String())
	}
	s.mu.Unlock()
	if got := f.waitAsked(t, 1); len(got) != 1 || got[0] != device.ADBOff {
		t.Errorf("the device was asked for %v, want only [Off]: every position in between is a port nobody wanted", got)
	}
}

// netd rebuilds the INPUT chain and discards what it finds, so a rule added
// once does not stay. This holds the wiring: the poll is what puts it back, on
// every turn. Whether there is a port worth holding open is the device's
// question, and TestHoldADBOpenAsksWhetherADBDIsListening in internal/device
// holds that half -- a stub here would answer it for the code under test.
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
	// The poll asserts once before its first wait, and this channel is
	// buffered, so that first call is already sitting in it. Drained here:
	// without this the receive below is satisfied by the startup call, and
	// moving the re-assert out of the loop entirely would leave this green.
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never asserted the rule at startup")
	}

	s.mu.Lock()
	s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
	s.mu.Unlock()
	f.waitAsked(t, 1)

	// Woken rather than waited for. The rule is put back on a turn of this
	// poll, and a test that sat out a real one would spend a minute proving
	// something a wake proves in a moment. device.SetADBMode opens the port
	// itself, so what this holds is the re-assert after netd's rebuild.
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

// Changing position restarts adbd, which drops every live adb session. Home
// Assistant resends a select's own value readily enough, and a peer holding the
// key can do it on a connection it already has -- which the eight slots do not
// bound and the poll's wake gap does not either, since neither is on this path.
// So a command asking for where the device already is stops here.
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

	// Idle first, so the in-flight half of the guard cannot be what turns the
	// repeats away: what is under test here is the published state, which is
	// what the poll would have put there by the time Home Assistant could send
	// a second command at all.
	waitADBIdle(t, s)
	s.publish("sensors", s.readTicked())

	for range 3 {
		s.mu.Lock()
		s.setADBLocked(&conn{sock: fakeAddr{}}, device.ADBInsecure.String())
		s.mu.Unlock()
	}
	time.Sleep(50 * time.Millisecond)
	if got := f.askedFor(); len(got) != 1 {
		t.Errorf("the device was asked %d times for a position it was already in: %v", len(got), got)
	}
}

// The same guard while the worker is mid-restart, which is the window the
// published state cannot answer for: it still says where the device was, and
// the worker has already taken the pending mode off the slot. A second command
// naming that mode has asked for nothing new, and letting it through queues
// another adbd restart behind the one in flight -- which a peer can keep doing
// on a connection it already holds.
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

	// Wait until the worker is inside the apply: pending has been taken, so
	// only the in-flight half of the guard can turn the next command away.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		inFlight := s.adbWorking && !s.adbHasPending
		s.mu.Unlock()
		if inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never reached the apply")
		}
		time.Sleep(time.Millisecond)
	}

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
	time.Sleep(50 * time.Millisecond)
	if got := f.askedFor(); len(got) != 1 {
		t.Errorf("adbd was restarted %d times for one position: %v", len(got), got)
	}
}

func waitADBIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.adbWorking || s.adbHasPending
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the adb worker never went idle")
}

// adbd goes down on its own schedule after ctl.restart, so the sensor poll can
// find it still listening and put the rule back after the close took it out.
// Once adbd is gone nothing else would ever remove it: the mode reads Off, so
// no poll re-asserts it, the guard turns away a repeated Off, and uninstall.sh
// leaves this port alone on purpose.
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.denies
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("the rule was never deleted again after the port was closed")
}

// A position that half-took must stay askable. SetADBMode deletes the rule
// first and reports that failure last, so the properties can succeed while the
// rule stays behind -- and the device then reads Off, which is what the no-op
// guard would compare against for ever.
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

// "adbd is listening" lags a restart it was asked for, so a re-assert landing
// inside a transition puts back the rule the worker has just taken out.
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
	s.adbWorking = true // a worker in flight, without running one
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

// A select carries no missing_state, so a mode that could not be read is not
// published at all: the choice is between the last value Home Assistant holds
// and one invented here, and the invented one says the port is shut.
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
