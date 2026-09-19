package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeGuard struct {
	mu     sync.Mutex
	press  chan struct{}
	closed bool
}

func newFakeGuard() *fakeGuard { return &fakeGuard{press: make(chan struct{}, 1)} }

func (g *fakeGuard) Wait() error {
	if _, ok := <-g.press; !ok {
		return errors.New("guard closed")
	}
	return nil
}

func (g *fakeGuard) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	close(g.press)
}

func (g *fakeGuard) shut() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

type fakeMute struct {
	mu    sync.Mutex
	on    bool
	known bool
	stuck bool
	sets  []bool
	err   error
}

func wireMute(t *testing.T, m *fakeMute, open func() (volumeGuard, error)) {
	t.Helper()
	wasSet, wasLevel, wasGuard := muteSetter, muteLevel, muteGuard
	muteSetter = func(on bool) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.sets = append(m.sets, on)
		if m.err != nil {
			return m.err
		}
		if m.stuck && !on {
			return nil
		}
		m.on = on
		return nil
	}
	muteLevel = func() (int, bool, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return 15, m.on, m.known
	}
	muteGuard = open
	t.Cleanup(func() {
		muted.mu.Lock()
		muted.guard, muted.lost = nil, 0
		muteSetter, muteLevel, muteGuard = wasSet, wasLevel, wasGuard
		muted.mu.Unlock()
	})
}

func heldGuard() volumeGuard {
	muted.mu.Lock()
	defer muted.mu.Unlock()
	return muted.guard
}

func TestADotThatCameUpMutedHoldsTheVolumeKeys(t *testing.T) {
	m := &fakeMute{on: true, known: true}
	g := newFakeGuard()
	wireMute(t, m, func() (volumeGuard, error) { return g, nil })

	holdKeysIfMuted()

	if heldGuard() == nil {
		t.Error("a dot that came up muted holds no keys, so the first press leaves the" +
			" mute set and resets the level: the reconcile has to run after the api" +
			" server it reads the level through is stored")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sets) != 0 {
		t.Errorf("coming up muted set the mute %d times, want none: it is already set,"+
			" and Mute count is a reference count", len(m.sets))
	}
}

func TestADotThatCameUpUnmutedHoldsNothing(t *testing.T) {
	m := &fakeMute{on: false, known: true}
	wireMute(t, m, func() (volumeGuard, error) {
		t.Error("an unmuted dot opened a guard")
		return newFakeGuard(), nil
	})
	holdKeysIfMuted()
	if heldGuard() != nil {
		t.Error("an unmuted dot is holding the volume keys")
	}
}

func TestMutingTwiceDoesNotSetItTwice(t *testing.T) {
	m := &fakeMute{known: true}
	g := newFakeGuard()
	wireMute(t, m, func() (volumeGuard, error) { return g, nil })

	setSpeakerMute(true)
	setSpeakerMute(true)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sets) != 1 {
		t.Errorf("two mutes called setStreamMute %d times, want 1: Mute count is a"+
			" reference count, so the second leaves an unmute one short and the dot"+
			" muted with its keys free", len(m.sets))
	}
}

func TestAnUnmuteThatDidNotTakeKeepsTheKeysHeld(t *testing.T) {
	m := &fakeMute{known: true}
	g := newFakeGuard()
	wireMute(t, m, func() (volumeGuard, error) { return g, nil })

	setSpeakerMute(true)
	m.mu.Lock()
	m.err = errors.New("service call: refused")
	m.mu.Unlock()
	setSpeakerMute(false)

	if heldGuard() == nil {
		t.Error("an unmute that failed released the keys while the dot is still muted," +
			" so the next press resets the level")
	}
	if g.shut() {
		t.Error("the guard was closed by an unmute that did not take")
	}
}

func TestAPressLiftsTheMuteAndTheGuardGoes(t *testing.T) {
	m := &fakeMute{known: true}
	g := newFakeGuard()
	wireMute(t, m, func() (volumeGuard, error) { return g, nil })

	setSpeakerMute(true)
	g.press <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if heldGuard() == nil {
			m.mu.Lock()
			on := m.on
			m.mu.Unlock()
			if on {
				t.Error("the guard went but the dot is still muted")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("a press did not lift the mute")
}

func TestAnUnmuteTheDeviceIgnoredKeepsTheKeysHeld(t *testing.T) {
	m := &fakeMute{known: true}
	opens := 0
	var guards []*fakeGuard
	wireMute(t, m, func() (volumeGuard, error) {
		opens++
		g := newFakeGuard()
		guards = append(guards, g)
		return g, nil
	})

	setSpeakerMute(true)
	m.mu.Lock()
	m.stuck = true
	m.mu.Unlock()
	setSpeakerMute(false)

	if heldGuard() == nil {
		t.Error("an unmute the device reported as taken but did not apply left the keys" +
			" free while still muted: Mute count is a reference count, so one unmute" +
			" can succeed and leave the stream muted, and the next press then resets" +
			" the level with nothing holding the keys")
	}
	if opens != 1 || guards[0].shut() {
		t.Errorf("%d guards were opened and the first is closed=%v; the keys must never"+
			" be let go between an unmute landing and the read back that says whether"+
			" it took, or a press in that window resets the level", opens,
			guards[0].shut())
	}
}

func TestAGuardLostWhileMutedIsClosedAndTakenAgain(t *testing.T) {
	m := &fakeMute{known: true}
	var opened []*fakeGuard
	wireMute(t, m, func() (volumeGuard, error) {
		g := newFakeGuard()
		opened = append(opened, g)
		return g, nil
	})

	setSpeakerMute(true)
	first := opened[0]
	forgetVolumeGuard(first)

	if !first.shut() {
		t.Error("a guard whose node went away was dropped without being closed, so the" +
			" grab on the volume keys outlives it: every press stays swallowed with" +
			" nobody reading, and the next mute cannot grab against our own stale fd")
	}
	if len(opened) != 2 || heldGuard() != opened[1] {
		t.Errorf("%d guards were opened and the dot is holding %v; a guard lost while"+
			" still muted has to be taken again, or the keys are free and the next"+
			" press resets the level with nothing logged", len(opened), heldGuard())
	}
}

func TestAGuardLostAfterTheMuteWentIsNotTakenAgain(t *testing.T) {
	m := &fakeMute{known: true}
	opens := 0
	wireMute(t, m, func() (volumeGuard, error) {
		opens++
		return newFakeGuard(), nil
	})

	setSpeakerMute(true)
	held := heldGuard()
	m.mu.Lock()
	m.on = false
	m.mu.Unlock()
	forgetVolumeGuard(held)

	if opens != 1 || heldGuard() != nil {
		t.Errorf("%d guards were opened and %v is held after a guard went while the dot"+
			" was unmuted; nothing should be grabbed when there is no mute to protect",
			opens, heldGuard())
	}
}

func TestAMuteBeforeTheLevelIsWiredHoldsNothing(t *testing.T) {
	m := &fakeMute{known: true}
	g := newFakeGuard()
	wireMute(t, m, func() (volumeGuard, error) { return g, nil })
	muteLevel = nil

	setSpeakerMute(true)
	holdKeysIfMuted()

	if heldGuard() != nil {
		t.Error("a mute arriving before the level was wired armed a guard against a" +
			" level nobody can read")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sets) != 0 {
		t.Errorf("the mute was set %d times with no level to read back, want none:"+
			" Mute count is a reference count, and one that cannot be read back is"+
			" one that cannot be undone", len(m.sets))
	}
}

type deadGuard struct{}

func (deadGuard) Wait() error { return errors.New("read: input/output error") }
func (deadGuard) Close()      {}

func TestKeysThatKeepGoingAwayAreNotTakenForever(t *testing.T) {
	m := &fakeMute{known: true}
	var mu sync.Mutex
	opens := 0
	wireMute(t, m, func() (volumeGuard, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		return deadGuard{}, nil
	})

	setSpeakerMute(true)
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if opens > volumeGuardTries+1 {
		t.Errorf("a node that fails every read was grabbed %d times in 200ms; taking it"+
			" again on every loss spins one core on open, ioctl and close for as long"+
			" as the mute is held", opens)
	}
	if opens < 2 {
		t.Errorf("a node lost while muted was taken %d times, want it retried", opens)
	}
}

func TestACleanUnmuteLetsTheNextMuteRetryAfresh(t *testing.T) {
	m := &fakeMute{known: true}
	var mu sync.Mutex
	opens := 0
	wireMute(t, m, func() (volumeGuard, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		return deadGuard{}, nil
	})

	setSpeakerMute(true)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	first := opens
	mu.Unlock()

	m.mu.Lock()
	m.on = false
	m.mu.Unlock()
	setSpeakerMute(false)
	setSpeakerMute(true)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if opens-first < 2 {
		t.Errorf("a fresh mute after a clean unmute opened %d guards, want it trying"+
			" again: the retry count is per mute rather than per process, or a dot"+
			" that lost its keys once never holds them again", opens-first)
	}
}
