package device

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetVolume(t *testing.T) {
	t.Helper()
	wasCommand, wasSettle, wasWatch := lipcCommand, volumeSettle, volumeWatchArgv
	volume.mu.Lock()
	volume.level, volume.levelOK, volume.muted, volume.muteOK = 0, false, false, false
	volume.mu.Unlock()
	t.Cleanup(func() {
		lipcCommand, volumeSettle, volumeWatchArgv = wasCommand, wasSettle, wasWatch
		volume.mu.Lock()
		volume.level, volume.levelOK, volume.muted, volume.muteOK = 0, false, false, false
		volume.mu.Unlock()
	})
}

type lipcCalls struct {
	mu    sync.Mutex
	lines []string
}

func (c *lipcCalls) record(argv []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, strings.Join(argv, " "))
}

func (c *lipcCalls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func TestAVolumeIsSetThroughAlexaAtTheLevelForTheStep(t *testing.T) {
	resetVolume(t)
	volumeEvent("MainVolumeChanged 33 3")
	volumeEvent("MuteChanged 0 33")
	var calls lipcCalls
	lipcCommand = func(_ context.Context, argv []string) ([]byte, error) {
		calls.record(argv)
		go volumeEvent("MainVolumeChanged 47 3")
		return nil, nil
	}

	if err := SetMusicVolume(14); err != nil {
		t.Fatalf("SetMusicVolume: %v", err)
	}
	want := "/system/bin/lipc-set-prop -i -q com.amazon.puffin AvsVolume 47"
	if got := calls.all(); len(got) != 1 || got[0] != want {
		t.Errorf("the calls were %q, want only %q: audiod's own level leaves Alexa, the"+
			" light ring and the cloud on the old one", got, want)
	}
	if v := MusicVolumes(); v.Step != 14 || v.Percent != 47 {
		t.Errorf("after the set the reading is step %d at %v%%, want 14 at 47%%: the set"+
			" returns once the follower has seen it, so a read back is current", v.Step, v.Percent)
	}
}

func TestASetThatNothingReportsStillReturns(t *testing.T) {
	resetVolume(t)
	volumeSettle = 50 * time.Millisecond
	lipcCommand = func(context.Context, []string) ([]byte, error) { return nil, nil }

	done := make(chan error, 1)
	go func() { done <- SetMusicVolume(14) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a set lipc took was reported as %v; the read back reports the level", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a set with no event after it never returned, so the volume worker stops")
	}
}

func TestAVolumeOrMuteLipcRefusedIsAnError(t *testing.T) {
	resetVolume(t)
	lipcCommand = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("exit status 8")
	}
	if err := SetMusicVolume(14); err == nil {
		t.Error("a refused volume set was taken as one that landed")
	}
	if err := SetMusicMute(true); err == nil {
		t.Error("a refused mute was taken as one that landed")
	}
}

func TestAMuteIsSetOnAudiod(t *testing.T) {
	resetVolume(t)
	volumeSettle = 50 * time.Millisecond
	var calls lipcCalls
	lipcCommand = func(_ context.Context, argv []string) ([]byte, error) {
		calls.record(argv)
		return nil, nil
	}
	_ = SetMusicMute(true)
	_ = SetMusicMute(false)
	want := []string{
		"/system/bin/lipc-set-prop -i -q com.doppler.audiod Mute 1",
		"/system/bin/lipc-set-prop -i -q com.doppler.audiod Mute 0",
	}
	if got := calls.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the calls were %q, want %q", got, want)
	}
}

func TestTheEventsCarryTheLevelAndTheMute(t *testing.T) {
	resetVolume(t)
	volumeEvent("MainVolumeChanged 50 3")
	if v := MusicVolumes(); v.OK {
		t.Errorf("a level with no mute known read as %+v; a mute nobody has read is not off", v)
	}
	volumeEvent("MuteChanged 1 43")
	v := MusicVolumes()
	if !v.OK || !v.Muted || v.Step != 13 || v.Max != VolumeSteps {
		t.Errorf("after a mute carrying level 43 the reading is %+v, want muted at step 13 of %d",
			v, VolumeSteps)
	}
	if v.Percent != 0 {
		t.Errorf("a muted Dot reads %v%%, want 0: zero is what can be heard", v.Percent)
	}
	volumeEvent("MuteChanged 0 43")
	if v := MusicVolumes(); v.Muted || v.Percent != 43 {
		t.Errorf("after the unmute the reading is %+v, want 43%% and not muted", v)
	}
}

func TestAnEventThatDoesNotParseChangesNothing(t *testing.T) {
	resetVolume(t)
	volumeEvent("MainVolumeChanged 50 3")
	volumeEvent("MuteChanged 0 50")
	for _, line := range []string{
		"", "MainVolumeChanged", "MainVolumeChanged loud", "TTSVolumeChanged 10",
		"timeout pid 1 signal 15",
	} {
		volumeEvent(line)
	}
	if v := MusicVolumes(); v.Percent != 50 || v.Muted {
		t.Errorf("after lines that are not volume events the reading is %+v, want 50%%", v)
	}
}

func TestALevelOffTheScaleIsClamped(t *testing.T) {
	resetVolume(t)
	volumeEvent("MuteChanged 0 0")
	volumeEvent("MainVolumeChanged 150 3")
	if v := MusicVolumes(); v.Percent != 100 || v.Step != VolumeSteps {
		t.Errorf("a level of 150 reads %+v, want 100%%", v)
	}
	volumeEvent("MainVolumeChanged -5 3")
	if v := MusicVolumes(); v.Percent != 0 || v.Step != 0 {
		t.Errorf("a level of -5 reads %+v, want 0%%", v)
	}
}

func TestEveryStepSurvivesTheTripThroughAlexasLevel(t *testing.T) {
	for step := 0; step <= VolumeSteps; step++ {
		if got := levelStep(stepLevel(step)); got != step {
			t.Errorf("step %d sets level %d, which reads back as step %d", step,
				stepLevel(step), got)
		}
	}
	for level, step := range map[int]int{40: 12, 43: 13, 47: 14, 50: 15, 53: 16} {
		if got := levelStep(level); got != step {
			t.Errorf("level %d, where a volume key lands, reads as step %d, want %d",
				level, got, step)
		}
	}
}

func TestAReadOverTakenByAnEventIsDropped(t *testing.T) {
	resetVolume(t)
	volumeEvent("MuteChanged 0 30")
	lipcCommand = func(_ context.Context, argv []string) ([]byte, error) {
		volumeEvent("MainVolumeChanged 60 3")
		if argv[len(argv)-1] == "Mute" {
			return []byte("0\n"), nil
		}
		return []byte("30\n"), nil
	}
	readVolume()
	if v := MusicVolumes(); v.Percent != 60 {
		t.Errorf("a read that started before an event left %v%%, want the event's 60%%", v.Percent)
	}
}

func TestALevelEventDuringTheSeedLeavesTheMuteToTheSeed(t *testing.T) {
	resetVolume(t)
	lipcCommand = func(_ context.Context, argv []string) ([]byte, error) {
		if argv[len(argv)-1] == "Mute" {
			return []byte("1\n"), nil
		}
		volumeEvent("MainVolumeChanged 60 3")
		return []byte("30\n"), nil
	}
	readVolume()
	v := MusicVolumes()
	if !v.OK || !v.Muted {
		t.Errorf("a key press during the seed left %+v, want muted: the event said nothing"+
			" about the mute, so dropping the seed's mute leaves no reading until the"+
			" minute read, and a dot that came up muted holds no keys", v)
	}
	if v.Step != levelStep(60) {
		t.Errorf("the seed's level replaced the newer event's: step %d, want %d", v.Step,
			levelStep(60))
	}
}

func TestAFollowerThatFailsSaysHow(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	resetVolume(t)
	lipcCommand = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("exit status 3")
	}
	volumeWatchArgv = []string{"sh", "-c", "exit 3"}
	err := followVolume()
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("a follower that exited 3 returned %v; the status tells no publisher"+
			" from anything else", err)
	}
}

func TestAReadSeedsWhatNoEventHasSaid(t *testing.T) {
	resetVolume(t)
	var calls lipcCalls
	lipcCommand = func(_ context.Context, argv []string) ([]byte, error) {
		calls.record(argv)
		if argv[len(argv)-1] == "Mute" {
			return []byte("1\n"), nil
		}
		return []byte("47\n"), nil
	}
	readVolume()
	if v := MusicVolumes(); !v.OK || !v.Muted || v.Step != 14 {
		t.Errorf("the seed read gives %+v, want muted at step 14", v)
	}
	want := []string{
		"/system/bin/lipc-get-prop -i -q com.doppler.audiod MainVolume",
		"/system/bin/lipc-get-prop -i -q com.doppler.audiod Mute",
	}
	if got := calls.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the reads were %q, want %q: the events come from audiod, so the seed"+
			" reads audiod too", got, want)
	}
}

func TestTheFollowerReadsEventsAndForgetsWhenItEnds(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	resetVolume(t)
	lipcCommand = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("exit status 3")
	}
	volumeWatchArgv = []string{"sh", "-c",
		"echo 'MainVolumeChanged 47 3'; echo 'MuteChanged 1 47'; sleep 0.3"}

	done := make(chan error, 1)
	go func() { done <- followVolume() }()
	if !AwaitVolume(5 * time.Second) {
		t.Fatal("the follower's events never reached the reading")
	}
	if v := MusicVolumes(); !v.Muted || v.Step != 14 {
		t.Errorf("the follower left %+v, want muted at step 14", v)
	}
	if err := <-done; err == nil {
		t.Error("a follower that ended returned no error, so nothing says the volume went stale")
	}
	forgetVolume()
	if v := MusicVolumes(); v.OK {
		t.Errorf("after the follower ended the reading is %+v, want none: a level nobody"+
			" is watching goes stale in silence", v)
	}
}
