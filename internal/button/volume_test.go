package button

import (
	"errors"
	"testing"
)

func TestStepsEmitOneDownAndUpPerStep(t *testing.T) {
	for _, tt := range []struct {
		name string
		up   bool
		n    int
		want []event
	}{
		{"one step up", true, 1, []event{{KeyVolumeUp, 1}, {KeyVolumeUp, 0}}},
		{"one step down", false, 1, []event{{KeyVolumeDown, 1}, {KeyVolumeDown, 0}}},
		{"three steps down", false, 3, []event{
			{KeyVolumeDown, 1}, {KeyVolumeDown, 0},
			{KeyVolumeDown, 1}, {KeyVolumeDown, 0},
			{KeyVolumeDown, 1}, {KeyVolumeDown, 0},
		}},
		{"no steps", true, 0, nil},
		{"a negative count is no steps", true, -2, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []event
			err := steps(func(code uint16, value int32) error {
				got = append(got, event{code, value})
				return nil
			}, 0, tt.up, tt.n)
			if err != nil {
				t.Fatalf("steps: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("emitted %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("emitted %v, want %v", got, tt.want)
				}
			}
		})
	}
}

type event struct {
	code  uint16
	value int32
}

func TestAVolumeStepThatSticksStopsTheRest(t *testing.T) {
	calls := 0
	err := steps(func(uint16, int32) error {
		calls++
		if calls == 2 {
			return errors.New("uinput: write: bad file descriptor")
		}
		return nil
	}, 0, false, 5)
	if !errors.Is(err, ErrKeyStuck) {
		t.Errorf("a volume step whose release failed gave %v, want an ErrKeyStuck: a volume "+
			"key left down is half of Alexa's advanced reset combination", err)
	}
	if calls != 2 {
		t.Errorf("the run made %d calls, want to stop at the step that failed", calls)
	}
}
