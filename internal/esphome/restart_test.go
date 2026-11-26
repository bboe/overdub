package esphome

import (
	"testing"
	"time"
)

func TestTheRestartButtonIsListedOnlyWhenWired(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if _, _, ok := listedEntity(t, s, msgListButton, "restart"); ok {
		t.Error("a restart button was listed with nothing behind it")
	}
	s.UseRestart(func() {})
	e, _, ok := listedEntity(t, s, msgListButton, "restart")
	if !ok {
		t.Fatal("no restart button was listed")
	}
	if string(e[1].data) != "restart" || uint32(e[2].num) != s.keyRestart || string(e[3].data) != "Restart" ||
		e[6].num != 0 || e[7].num != entityCategoryConfig || string(e[8].data) != "restart" {
		t.Errorf("the restart button was listed as %q key %d %q, category %d, class %q",
			e[1].data, e[2].num, e[3].data, e[7].num, e[8].data)
	}
}

func TestOnlyThePressOfTheRestartButtonRestarts(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	restarted := make(chan struct{}, 4)
	s.UseRestart(func() { restarted <- struct{}{} })
	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	for _, key := range []uint32{s.keySendspin, s.keyUpdate, s.keyRestart} {
		var msg pb
		msg.fixed32(1, key)
		if err := s.handle(c, msgButtonCommand, msg.b); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the restart button's press did not restart the Dot")
	}
	select {
	case <-restarted:
		t.Error("a press on another key restarted the Dot too")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestARestartPressWithNothingWiredIsIgnored(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	var msg pb
	msg.fixed32(1, s.keyRestart)
	if err := s.handle(c, msgButtonCommand, msg.b); err != nil {
		t.Fatal(err)
	}
}
