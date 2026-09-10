package device

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

const (
	micReplyLive  = "Result: Parcel(00000000   '....')\n"
	micReplyMuted = "Result: Parcel(00000001   '....')\n"
)

func TestParseMicMuteReadsTheReplyParcel(t *testing.T) {
	for _, tt := range []struct {
		name  string
		out   string
		muted bool
		ok    bool
	}{
		{"live", micReplyLive, false, true},
		{"muted", micReplyMuted, true, true},
		{"a value no bool produces", "Result: Parcel(00000002   '....')\n", false, false},
		{"a short word", "Result: Parcel(0001 '..')\n", false, false},
		{"an error from the service", "service: Service media.audio_flinger does not exist\n", false, false},
		{"nothing at all", "", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			muted, ok := parseMicMute(tt.out)
			if muted != tt.muted || ok != tt.ok {
				t.Errorf("parseMicMute(%q) = %v, %v; want %v, %v", tt.out, muted, ok, tt.muted, tt.ok)
			}
		})
	}
}

func TestAMicReadThatFailedIsNoReading(t *testing.T) {
	defer func(c func(context.Context) ([]byte, error)) { micCommand = c }(micCommand)

	micCommand = func(context.Context) ([]byte, error) {
		return []byte(micReplyMuted), errors.New("killed")
	}
	if muted, ok := MicMuted(); ok || muted {
		t.Errorf("MicMuted() = %v, %v; want a reading nobody took", muted, ok)
	}

	micCommand = func(context.Context) ([]byte, error) { return []byte(micReplyMuted), nil }
	if muted, ok := MicMuted(); !ok || !muted {
		t.Errorf("MicMuted() = %v, %v; want true, true", muted, ok)
	}
}

func TestTheMicReadsTheFlingerMuteTransaction(t *testing.T) {
	want := []string{"/system/bin/service", "call", "media.audio_flinger", "18"}
	if len(micArgv) != len(want) {
		t.Fatalf("micArgv is %v, want %v", micArgv, want)
	}
	for i := range want {
		if micArgv[i] != want[i] {
			t.Errorf("micArgv is %v, want %v", micArgv, want)
			break
		}
	}
}

func TestOneMicReadCannotOutlastItsBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	wasArgv, wasTimeout, wasDelay := micArgv, micReadTimeout, micWaitDelay
	defer func() {
		micArgv, micReadTimeout, micWaitDelay = wasArgv, wasTimeout, wasDelay
	}()

	micReadTimeout, micWaitDelay = 100*time.Millisecond, 400*time.Millisecond
	micArgv = []string{"sh", "-c", "sleep 30 & sleep 30"}

	start := time.Now()
	muted, ok := MicMuted()
	elapsed := time.Since(start)

	if ok || muted {
		t.Errorf("a read that never answered reported muted=%v ok=%v", muted, ok)
	}
	if elapsed < micReadTimeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	if limit := MicReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, MicReadBudget())
	}
}
