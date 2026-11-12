package esphome

import (
	"math"
	"testing"
)

func updateIn(entities []map[int]pbField) map[int]pbField {
	for _, e := range entities {
		if int(e[0].num) == msgListUpdate {
			return e
		}
	}
	return nil
}

func TestTheUpdateEntityIsListedOnlyWhenWired(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if updateIn(listed(t, s)) != nil {
		t.Error("an update entity was listed with no updater behind it")
	}
	s.UseUpdate(func() {}, func() {})
	e := updateIn(listed(t, s))
	if e == nil {
		t.Fatal("no update entity was listed")
	}
	if string(e[1].data) != "firmware" || uint32(e[2].num) != s.keyUpdate || string(e[3].data) != "Firmware" ||
		e[7].num != entityCategoryConfig || string(e[8].data) != "firmware" {
		t.Errorf("the update entity was listed as %q key %d %q, category %d, class %q",
			e[1].data, e[2].num, e[3].data, e[7].num, e[8].data)
	}
}

func TestAnUpdateCommandReachesTheUpdater(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	var checks, installs int
	s.UseUpdate(func() { checks++ }, func() { installs++ })
	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	for _, cmd := range []struct {
		key     uint32
		command uint32
	}{
		{s.keyUpdate, updateCommandCheck},
		{s.keyUpdate, updateCommandInstall},
		{s.keyUpdate, 0},
		{s.keySendspin, updateCommandInstall},
	} {
		var msg pb
		msg.fixed32(1, cmd.key)
		msg.u32(2, cmd.command)
		if err := s.handle(c, msgUpdateCommand, msg.b); err != nil {
			t.Fatal(err)
		}
	}
	if checks != 1 || installs != 1 {
		t.Errorf("the updater saw %d checks and %d installs, want 1 of each", checks, installs)
	}
}

func TestAnUpdateCommandWithNoUpdaterIsIgnored(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	var msg pb
	msg.fixed32(1, s.keyUpdate)
	msg.u32(2, updateCommandInstall)
	if err := s.handle(c, msgUpdateCommand, msg.b); err != nil {
		t.Fatal(err)
	}
}

func TestTheUpdateStateCarriesWhatHomeAssistantReads(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	s.UseUpdate(func() {}, func() {})
	for _, want := range []UpdateState{
		{Current: "v0.2.0", Latest: "v0.3.0", Title: "overdub", Notes: "notes", URL: "https://x/y",
			InProgress: true, HasProgress: true, Progress: 42.5},
		{Current: "v0.2.0", Latest: "v0.3.0", Title: "overdub", InProgress: true},
		{Current: "v0.2.0", Latest: "v0.3.0", Title: "overdub", HasProgress: true, Progress: 7},
	} {
		s.SetUpdate(want)
		c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
		if err := s.handle(c, msgSubscribeStates, nil); err != nil {
			t.Fatal(err)
		}
		close(c.out)
		var fields map[int]pbField
		for f := range c.out {
			if f.msgType == msgUpdateState {
				fields = map[int]pbField{}
				if err := pbWalk(f.payload, func(p pbField) { fields[p.field] = p }); err != nil {
					t.Fatal(err)
				}
			}
		}
		if fields == nil {
			t.Fatal("a subscriber was sent no update state")
		}
		got := UpdateState{
			Current:     string(fields[6].data),
			Latest:      string(fields[7].data),
			Title:       string(fields[8].data),
			Notes:       string(fields[9].data),
			URL:         string(fields[10].data),
			InProgress:  fields[3].num != 0,
			HasProgress: fields[4].num != 0,
			Progress:    math.Float32frombits(uint32(fields[5].num)),
		}
		if uint32(fields[1].num) != s.keyUpdate || fields[2].num != 0 || got != want {
			t.Errorf("key %d state %+v, want key %d state %+v", fields[1].num, got, s.keyUpdate, want)
		}
	}
}
