package esphome

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestARouteChangeWakesTheNameRead(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	wired := func() (bool, bool) { return false, true }
	bluetooth := func() (bool, bool) { return true, true }
	unknown := func() (bool, bool) { return false, false }
	s.btRoute = wired

	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("the first route seen woke the name read; subscribing already wakes it")
	}
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("a route that did not change woke the poll that reads the speaker's name")
	}

	s.jack = func() (bool, bool) { return false, true }
	s.readLive()
	s.jack = func() (bool, bool) { return true, true }
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("the route moved between the jack and the speaker, which cannot change the " +
			"Bluetooth device, and woke the name read")
	}

	s.btRoute = unknown
	s.readLive()
	s.btRoute = wired
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Error("a route that could not be read, between two that agree, woke the name read")
	}

	s.btRoute = bluetooth
	s.readLive()
	if len(s.sensorWake) != 1 {
		t.Fatal("the route became bluetooth and the name read was left to the minute tick")
	}
	<-s.sensorWake
	s.readLive()
	if len(s.sensorWake) != 0 {
		t.Fatal("a speaker still connected woke the name read on every heavy tick")
	}

	s.btRoute = wired
	s.readLive()
	if len(s.sensorWake) != 1 {
		t.Fatal("the speaker went away and the name it left behind was not read again")
	}
	<-s.sensorWake

	s.btRoute = unknown
	s.readLive()
	s.btRoute = bluetooth
	s.readLive()
	if len(s.sensorWake) != 1 {
		t.Fatal("a route that could not be read hid the speaker that connected across it")
	}

	s.btRoute = wired
	done := make(chan struct{})
	go func() {
		s.readLive()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a wake the sensor poll had not yet taken blocked the live poll")
	}
}

func TestTheRouteIsForgottenWhenTheLastSubscriberGoes(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	var onBluetooth atomic.Bool
	s.btRoute = func() (bool, bool) { return onBluetooth.Load(), true }
	sub := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true}
	join := func() {
		s.mu.Lock()
		s.conns[sub] = struct{}{}
		s.mu.Unlock()
	}
	leave := func() {
		s.mu.Lock()
		delete(s.conns, sub)
		s.mu.Unlock()
	}
	t.Cleanup(leave)
	published := func(key uint32) (reading, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.published[key]
		return r, ok
	}
	until := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if done() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal(what)
	}

	join()
	go s.PollLive(5 * time.Millisecond)
	until("the poll never published the route", func() bool {
		r, ok := published(s.keyOutput)
		return ok && r.text == routeJack
	})

	leave()
	until("the poll never noticed the last subscriber go", func() bool {
		_, ok := published(s.keyPlaying)
		return !ok
	})
	onBluetooth.Store(true)
	select {
	case <-s.sensorWake:
	default:
	}

	join()
	until("the poll never published the route it found on return", func() bool {
		r, ok := published(s.keyOutput)
		return ok && r.text == routeBluetooth
	})
	if len(s.sensorWake) != 0 {
		t.Error("a subscriber's return woke the name read a second time; subscribing already " +
			"wakes it, and the route it was compared against was taken for somebody else")
	}
}

func TestPlayingIsMissingUntilSomethingSaysWhatPlays(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if r := s.readPlaying(); r.ok {
		t.Errorf("with nothing wired, playing read %q; it should be missing", r.text)
	}
	s.UsePlaying(func() string { return "dialog" })
	if r := s.readPlaying(); !r.ok || r.text != "dialog" || r.kind != kindTextSensor {
		t.Errorf("playing read %+v, want the text sensor's dialog", r)
	}
}

func TestAChangeInWhatPlaysWakesTheLivePoll(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	s.NotePlaying()
	s.NotePlaying()
	if len(s.liveWake) != 1 {
		t.Errorf("%d wakes queued, want 1: a change should wake the poll, and never block", len(s.liveWake))
	}
}
