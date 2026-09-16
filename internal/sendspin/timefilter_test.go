package sendspin

import (
	"math"
	"testing"
)

func feed(f *timeFilter, samples int, offset, maxError, start, step int64) {
	for i := range int64(samples) {
		f.Update(offset, maxError, start+i*step)
	}
}

func TestAFilterWithOneMeasurementHasNotConverged(t *testing.T) {
	f := newTimeFilter()
	f.Update(1_000_000, 2_000, 1_000_000)
	if f.Converged() {
		t.Error("one measurement cannot separate an offset from the noise around it")
	}
}

func TestAFilterConvergesOnASteadyOffset(t *testing.T) {
	const offset = 1_500_000
	f := newTimeFilter()
	feed(f, 20, offset, 2_000, 1_000_000, 1_000_000)

	if !f.Converged() {
		t.Fatal("twenty steady measurements left the filter unconverged")
	}
	got := f.ServerTime(20_000_000)
	if diff := got - (20_000_000 + offset); diff < -2_000 || diff > 2_000 {
		t.Errorf("server time is %d us from the offset it was fed, want within 2000", diff)
	}
	_, spread := f.state()
	if spread > 2_000 {
		t.Errorf("the filter reports a spread of %d us against a 2000 us measurement error", spread)
	}
}

func TestAFilterIgnoresAMeasurementThatDidNotMoveForward(t *testing.T) {
	f := newTimeFilter()
	f.Update(1_000_000, 2_000, 5_000_000)
	f.Update(1_000_000, 2_000, 5_000_000)

	if f.Converged() {
		t.Error("two measurements taken at one instant were both counted, so the" +
			" second divided by a zero interval")
	}
	if e := f.element(); math.IsNaN(e.offset) || math.IsInf(e.offset, 0) {
		t.Errorf("offset is %v after a zero interval", e.offset)
	}
}

func TestClientTimeUndoesServerTime(t *testing.T) {
	f := newTimeFilter()
	feed(f, 20, 1_500_000, 2_000, 1_000_000, 1_000_000)

	for _, at := range []int64{0, 20_000_000, 900_000_000} {
		if back := f.ClientTime(f.ServerTime(at)); back < at-1 || back > at+1 {
			t.Errorf("client time %d survived the round trip as %d", at, back)
		}
	}
}

func TestAnUnconvergedFilterAsksAsFastAsItMay(t *testing.T) {
	k := newClock()
	if d := k.every(); d != askFast {
		t.Errorf("an unconverged clock asks every %s, want %s", d, askFast)
	}
	feed(k.filter, 20, 1_000_000, 100, 1_000_000, 1_000_000)
	if d := k.every(); d != askCalm {
		t.Errorf("a clock settled to well under a millisecond asks every %s, want %s",
			d, askCalm)
	}
}
