package device

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"
)

func TestParseMicMuteReadsButtondsState(t *testing.T) {
	for _, tt := range []struct {
		name  string
		out   string
		muted bool
		ok    bool
	}{
		{"mic on", "8\n", false, true},
		{"mic off", "9\n", true, true},
		{"a state acebuttond does not publish", "7\n", false, false},
		{"an error from lipc", "lipc-get-prop: failed\n", false, false},
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

func TestTheMicIsTheDriversLatch(t *testing.T) {
	was := privacyDirs
	t.Cleanup(func() { privacyDirs = was })
	dir := t.TempDir()
	privacyDirs = []string{dir + "/missing", dir}
	for _, tt := range []struct {
		state string
		muted bool
		ok    bool
	}{
		{"0\n", false, true},
		{"1\n", true, true},
		{"2\n", false, false},
	} {
		if err := os.WriteFile(dir+"/state", []byte(tt.state), 0o644); err != nil {
			t.Fatal(err)
		}
		if muted, ok := MicMuted(); muted != tt.muted || ok != tt.ok {
			t.Errorf("state %q read as %v, %v; want %v, %v: the driver is the truth, and"+
				" acebuttond's muteState lags it or misses a latch", tt.state, muted, ok, tt.muted, tt.ok)
		}
	}
	privacyDirs = []string{dir + "/missing"}
	if _, ok := MicMuted(); ok {
		t.Error("a Dot with no privacy driver read as a state")
	}
}

func TestAcebuttondIsAskedOverLIPC(t *testing.T) {
	defer func(c func(context.Context, []string) ([]byte, error)) { lipcCommand = c }(lipcCommand)
	var asked []string
	lipcCommand = func(_ context.Context, argv []string) ([]byte, error) {
		asked = argv
		return []byte("9\n"), errors.New("killed")
	}
	if buttondMuted() {
		t.Error("a read that failed said acebuttond had adopted the mute")
	}
	want := []string{lipcGetPath, "-i", "-q", "com.doppler.buttond", "muteState"}
	if !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	lipcCommand = func(context.Context, []string) ([]byte, error) { return []byte("9\n"), nil }
	if !buttondMuted() {
		t.Error("muteState 9 did not read as adopted")
	}
}

func fakePrivacy(t *testing.T) (enable string, restarts *[]string) {
	t.Helper()
	wasDirs, wasSet, wasCmd, wasWait, wasNudge := privacyDirs, setProp, lipcCommand, micAdoptWait, micNudgeWait
	t.Cleanup(func() {
		privacyDirs, setProp, lipcCommand, micAdoptWait, micNudgeWait = wasDirs, wasSet, wasCmd, wasWait, wasNudge
	})
	dir := t.TempDir()
	privacyDirs = []string{dir + "/missing", dir}
	enable = dir + "/enable"
	if err := os.WriteFile(enable, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	var log []string
	restarts = &log
	state := "8\n"
	setProp = func(key, value string) error {
		log = append(log, key+"="+value)
		if b, _ := os.ReadFile(enable); string(b) == "1" {
			state = "9\n"
		}
		return nil
	}
	lipcCommand = func(context.Context, []string) ([]byte, error) { return []byte(state), nil }
	micAdoptWait, micNudgeWait = 300*time.Millisecond, 200*time.Millisecond
	return enable, restarts
}

func TestAMuteKeyTellsAcebuttondWithoutARestart(t *testing.T) {
	enable, restarts := fakePrivacy(t)
	nudged := false
	lipcState := "8\n"
	lipcCommand = func(context.Context, []string) ([]byte, error) { return []byte(lipcState), nil }
	if err := SetMicMute(true, func() error {
		nudged = true
		if b, _ := os.ReadFile(enable); string(b) == "1" {
			lipcState = "9\n"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !nudged || len(*restarts) != 0 {
		t.Errorf("nudged=%v, restarted %v; want a key and no restart: a restart kills"+
			" acebuttond as Alexa plays the mic-off tone, and the tone went silent", nudged, *restarts)
	}
}

func TestMutingLatchesTheDriverAndHasAcebuttondAdoptIt(t *testing.T) {
	enable, restarts := fakePrivacy(t)
	if err := SetMicMute(true, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(enable); string(b) != "1" {
		t.Errorf("enable holds %q, want 1: the driver latches the microphones off there", b)
	}
	if !slices.Equal(*restarts, []string{"ctl.restart=acebuttond"}) {
		t.Errorf("restarted %v, want acebuttond once no key reached it: it reads the driver"+
			" only at its start and on a press, so without either the ring stays dark", *restarts)
	}
}

func TestUnmutingIsRefusedWithoutTouchingTheDriver(t *testing.T) {
	enable, restarts := fakePrivacy(t)
	if err := SetMicMute(false, func() error { t.Error("an unmute pressed the key"); return nil }); !errors.Is(err, ErrMicButtonOnly) {
		t.Errorf("an unmute gave %v, want ErrMicButtonOnly: no write leaves privacy", err)
	}
	if b, _ := os.ReadFile(enable); string(b) != "0" || len(*restarts) != 0 {
		t.Errorf("an unmute wrote %q and restarted %v", b, *restarts)
	}
}

func TestAMuteAcebuttondNeverAdoptsIsAnError(t *testing.T) {
	_, _ = fakePrivacy(t)
	lipcCommand = func(context.Context, []string) ([]byte, error) { return []byte("8\n"), nil }
	if err := SetMicMute(true, nil); err == nil {
		t.Error("a mute muteState never showed was reported as done")
	}
}

func TestAFailedKeyFallsBackToARestart(t *testing.T) {
	_, restarts := fakePrivacy(t)
	if err := SetMicMute(true, func() error { return errors.New("uinput: write failed") }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*restarts, []string{"ctl.restart=acebuttond"}) {
		t.Errorf("restarted %v after the key failed, want acebuttond: the driver is latched"+
			" either way, and without it the ring stays dark", *restarts)
	}
}
