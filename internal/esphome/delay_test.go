package esphome

import (
	"math"
	"testing"
)

func numberIn(entities []map[int]pbField, objectID string) map[int]pbField {
	for _, e := range entities {
		if int(e[0].num) == msgListNumber && string(e[1].data) == objectID {
			return e
		}
	}
	return nil
}

func float(f pbField) float32 { return math.Float32frombits(uint32(f.num)) }

func delayCommand(key uint32, ms float32) []byte {
	var cmd pb
	cmd.fixed32(1, key)
	cmd.float(2, ms)
	return cmd.b
}

func TestTheSendspinDelayNumberIsListedTheWayHomeAssistantReadsIt(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if found := numberIn(listed(t, s), "sendspin_output_delay"); found != nil {
		t.Error("an output delay was listed before a Sendspin client was wired up")
	}

	const maxMS = 5000
	s.UseSendspinDelay(maxMS, func() int { return 900 }, func(int) {})
	n := numberIn(listed(t, s), "sendspin_output_delay")
	if n == nil {
		t.Fatal("no output delay was listed, so Home Assistant never offers the control")
	}
	if got := uint32(n[2].num); got != s.keyDelay {
		t.Errorf("the listed number carries key %d, want %d: a command for it would be"+
			" matched against a different entity", got, s.keyDelay)
	}
	if got := uint32(n[2].num); got != entityKey(string(n[1].data)) {
		t.Errorf("the number is listed as %q and keyed on something else (%d): every"+
			" other entity here keys on its own object_id, and the one that does not is"+
			" the one a later reader corrects into a different entity",
			string(n[1].data), got)
	}
	for _, tt := range []struct {
		field int
		want  float32
	}{{6, 0}, {7, maxMS}, {8, delayStepMS}} {
		if got := float(n[tt.field]); got != tt.want {
			t.Errorf("field %d of the number is %v, want %v", tt.field, got, tt.want)
		}
	}
	if got := string(n[11].data); got != delayUnit {
		t.Errorf("the number is listed in %q, want %q: an unlabelled figure on a card"+
			" beside a volume percentage says nothing about what it sets", got, delayUnit)
	}
	if got := n[10].num; got != entityCategoryConfig {
		t.Errorf("the number is in entity category %d, want the config category %d: this"+
			" is a setting rather than a reading, and a primary control puts it on the"+
			" card beside the volume", got, entityCategoryConfig)
	}
	if got := n[12].num; got != numberModeBox {
		t.Errorf("the number is listed in mode %d, want the box %d: a slider sends a"+
			" value for every step it is dragged through, and each one is a property"+
			" write and a client/state to whatever server is playing", got, numberModeBox)
	}
	if got := string(n[5].data); got != delayIcon {
		t.Errorf("the number carries icon %q, want %q", got, delayIcon)
	}
}

func TestADelayCommandReachesTheDelayAndNothingElse(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	var told []int
	s.UseSendspinDelay(5000, func() int { return 0 }, func(ms int) { told = append(told, ms) })

	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	if err := s.handle(c, msgNumberCommand, delayCommand(s.keyDelay, 900)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(told) != 1 || told[0] != 900 {
		t.Fatalf("the delay was told %v, want one 900", told)
	}

	if err := s.handle(c, msgNumberCommand, delayCommand(s.keyVolume, 1200)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(told) != 1 {
		t.Errorf("a command for another entity's key reached the output delay: %v", told)
	}
}

func TestADelayCommandIsOnlyReadAsAFloat(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	var told []int
	s.UseSendspinDelay(5000, func() int { return 900 }, func(ms int) { told = append(told, ms) })

	var varint pb
	varint.fixed32(1, s.keyDelay)
	varint.u32(2, 1200)

	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	if err := s.handle(c, msgNumberCommand, varint.b); err != nil {
		t.Fatalf("handle: %v", err)
	}
	for _, nonsense := range []float32{
		float32(math.NaN()),
		float32(math.Inf(1)),
		float32(math.Inf(-1)),
	} {
		if err := s.handle(c, msgNumberCommand, delayCommand(s.keyDelay, nonsense)); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if len(told) != 0 {
		t.Errorf("the delay was set to %v by a command carrying no usable figure: a field"+
			" sent as a varint is not a float that happens to be zero, and taking one as"+
			" zero moves this player's audio by a second on a malformed frame", told)
	}
}

func TestADelayCommandIsHeldToWhatTheEntityOffers(t *testing.T) {
	const maxMS = 5000
	for _, tt := range []struct {
		asked float32
		want  int
	}{
		{-1, 0},
		{-9000, 0},
		{maxMS + 1, maxMS},
		{99999, maxMS},
		{1499.6, 1500},
	} {
		s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
		var told []int
		s.UseSendspinDelay(maxMS, func() int { return 900 }, func(ms int) { told = append(told, ms) })

		c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
		if err := s.handle(c, msgNumberCommand, delayCommand(s.keyDelay, tt.asked)); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if len(told) != 1 || told[0] != tt.want {
			t.Errorf("a command for %v ms was passed on as %v, want %d: the range is"+
				" Sendspin's own and a figure outside it is refused there, so passing"+
				" one on sends the operator's figure somewhere it will not land",
				tt.asked, told, tt.want)
		}
	}
}

func TestTheDelayEntityReportsWhateverSetTheDelay(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	stubSensors(s)
	ms := 0
	s.UseSendspinDelay(5000, func() int { return ms }, func(int) {})

	got, ok := numberReading(s.readLive(), s.keyDelay)
	if !ok {
		t.Fatal("the live poll never reads the output delay, so the control Home" +
			" Assistant draws has no state at all")
	}
	if got.value != 0 || !got.ok {
		t.Errorf("the delay read as %v (ok=%v), want a known 0", got.value, got.ok)
	}

	ms = 1500
	got, _ = numberReading(s.readLive(), s.keyDelay)
	if got.value != 1500 {
		t.Errorf("a delay a server set read as %v, want 1500: the figure is one value"+
			" with two writers, so a server moving it has to show up in Home Assistant"+
			" rather than leaving the control saying what it last set", got.value)
	}

	c := &conn{out: make(chan frame, 4), sock: fakeAddr{}}
	if err := s.sendSensorsAt(c, []reading{got}); err != nil {
		t.Fatalf("sendSensorsAt: %v", err)
	}
	sent := <-c.out
	if sent.msgType != msgNumberState {
		t.Fatalf("the delay went out as message %d, want the %d a number's state is:"+
			" Home Assistant reads a state by the message it arrived in, so a sensor"+
			" state for this key reaches an entity that is not there and the control"+
			" never shows a figure at all", sent.msgType, msgNumberState)
	}
	key, value, missing := sensorReading(t, sent.msgType, sent.payload)
	if key != s.keyDelay || value != 1500 || missing {
		t.Errorf("the number state carries key %d value %v missing %v", key, value, missing)
	}
}

func numberReading(readings []reading, key uint32) (reading, bool) {
	for _, r := range readings {
		if r.key == key && r.kind == kindNumber {
			return r, true
		}
	}
	return reading{}, false
}

func TestTheSameDelaySentTwiceIsNeitherWrittenNorNoted(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	ms := 0
	var told []int
	s.UseSendspinDelay(5000, func() int { return ms }, func(want int) {
		told = append(told, want)
		ms = want
	})

	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	if err := s.handle(c, msgNumberCommand, delayCommand(s.keyDelay, 900)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if c.noted == "" {
		t.Fatal("a command that moved the delay was not noted, so nothing says who set it")
	}
	c.noted = ""

	if err := s.handle(c, msgNumberCommand, delayCommand(s.keyDelay, 900)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(told) != 1 {
		t.Errorf("the delay was told %v, want one 900: a figure that is already held is"+
			" not a change, and passing it on writes a property and a client/state for"+
			" nothing", told)
	}
	if c.noted != "" {
		t.Errorf("a command that changed nothing was noted as %q: every line a peer"+
			" causes is a write to /data, so a controller that re-sends what it already"+
			" set spends this daemon's log budget on nothing", c.noted)
	}
}

func TestADelayCommandCarryingNoStateAtAllIsZero(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	var told []int
	s.UseSendspinDelay(5000, func() int { return 900 }, func(ms int) { told = append(told, ms) })

	var bare pb
	bare.fixed32(1, s.keyDelay)

	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	if err := s.handle(c, msgNumberCommand, bare.b); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(told) != 1 || told[0] != 0 {
		t.Errorf("a command carrying no state was passed on as %v, want one 0: proto3"+
			" leaves a zero-valued scalar off the wire, so the one figure the operator"+
			" cannot send is the bottom of the range the entity itself offers", told)
	}
}

func TestAFigureSentWhileAnotherIsStillQueuedIsNotDroppedAsUnchanged(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	var told []int
	applied := 0
	s.UseSendspinDelay(5000, func() int { return applied }, func(ms int) {
		told = append(told, ms)
	})

	c := &conn{out: make(chan frame, 32), sock: fakeAddr{}}
	for _, ms := range []float32{900, 0} {
		if err := s.handle(c, msgNumberCommand, delayCommand(s.keyDelay, ms)); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	if len(told) != 2 || told[0] != 900 || told[1] != 0 {
		t.Errorf("two commands were passed on as %v, want 900 then 0: what the entity"+
			" reports is what has been applied, and a figure still queued behind a busy"+
			" worker is not applied yet, so reading the second command as a repeat drops"+
			" the operator's last instruction and lets the first one win", told)
	}
}
