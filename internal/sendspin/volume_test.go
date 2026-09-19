package sendspin

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

type fakeVolume struct {
	mu   sync.Mutex
	at   int
	ok   bool
	off  bool
	sets []int
}

func (v *fakeVolume) level() (int, bool, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.at, v.off, v.ok
}

func (v *fakeVolume) mute(on bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.off = on
}

func (v *fakeVolume) set(percent int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sets = append(v.sets, percent)
	v.at = percent
}

func (v *fakeVolume) took() []int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]int(nil), v.sets...)
}

func (v *fakeVolume) moveTo(percent int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.at = percent
}

func volumeClient(t *testing.T) (*Client, *fakeVolume) {
	t.Helper()
	c, _ := playingClient(t)
	v := &fakeVolume{at: 40, ok: true}
	c.Config.Level, c.Config.SetVolume = v.level, v.set
	return c, v
}

func volumeCommand(percent *int) serverCommand {
	return serverCommand{Player: &playerCommand{Command: commandVolume, Volume: percent}}
}

func TestHelloOffersVolumeOnlyWhenThereIsAVolumeToSet(t *testing.T) {
	if got := testConfig().playerCommands(); len(got) != 0 {
		t.Errorf("a dot with no volume wiring offers %v; a server that takes the offer"+
			" sends a command nothing here can carry out", got)
	}
	cfg := testConfig()
	v := &fakeVolume{at: 40, ok: true}
	cfg.Level, cfg.SetVolume = v.level, v.set
	h := cfg.hello()
	if got := h.PlayerSupport.SupportedCommands; len(got) != 1 || got[0] != commandVolume {
		t.Errorf("client/hello offers %v, want just %q: aiosendspin 9.1.1 reads volume"+
			" settability out of the hello support object, so a dot that stays silent"+
			" here is left out of every group volume the server works out", got,
			commandVolume)
	}
}

func TestTheStateCarriesTheVolumeAServerWouldOtherwiseHaveToGuess(t *testing.T) {
	ln := listenLocal(t)
	c, _ := volumeClient(t)
	serveOn(t, c, ln)
	_, _, state := bringUp(t, c, ln)

	if state.Player == nil || state.Player.Volume == nil {
		t.Fatal("client/state carries no volume while this player offers the command")
	}
	if *state.Player.Volume != 40 {
		t.Errorf("client/state reports a volume of %d, want 40", *state.Player.Volume)
	}
}

func TestAStateCarriesNoVolumeWhenThereIsNoneToRead(t *testing.T) {
	ln := listenLocal(t)
	c, v := volumeClient(t)
	v.ok = false
	serveOn(t, c, ln)
	_, _, state := bringUp(t, c, ln)

	if state.Player != nil && state.Player.Volume != nil {
		t.Errorf("client/state reported a volume of %d from a dump that could not be"+
			" read; an invented level is one a server will act on", *state.Player.Volume)
	}
}

func TestAVolumeAServerSetsReachesTheDevice(t *testing.T) {
	ln := listenLocal(t)
	c, v := volumeClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	want := 65
	peer.writeBinary(server.sealJSON(t, typeServerComm, volumeCommand(&want)))
	waitFor(t, "the volume to reach the device", func() bool { return len(v.took()) == 1 })

	if got := v.took()[0]; got != 65 {
		t.Errorf("the device was set to %d, want 65", got)
	}
}

func TestAVolumeOutsideWhatTheSpecAllowsIsHeldAtTheEndItPassed(t *testing.T) {
	for asked, want := range map[int]int{-1: 0, 0: 0, 50: 50, 100: 100, 101: 100} {
		if got := HoldVolume(asked); got != want {
			t.Errorf("a volume of %d is held at %d, want %d: aiosendspin refuses a"+
				" client/state outside 0 through 100, so the connection goes rather"+
				" than the number being clipped", asked, got, want)
		}
	}
}

func TestAVolumeChangedAtTheDeviceIsReportedWithoutBeingAsked(t *testing.T) {
	ln := listenLocal(t)
	c, v := volumeClient(t)
	c.volumeEvery = 10 * time.Millisecond
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	v.moveTo(70)

	deadline := time.Now().Add(3 * time.Second)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		if err := peer.conn.SetReadDeadline(time.Now().Add(left)); err != nil {
			t.Fatal(err)
		}
		_, err := peer.r.Peek(1)
		if err := peer.conn.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			break
		}
		kind, payload := readJSON(t, peer, server)
		if kind != typeClientState {
			continue
		}
		var state clientState
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		if state.Player != nil && state.Player.Volume != nil && *state.Player.Volume == 70 {
			return
		}
	}
	t.Error("a volume changed by the buttons on the dot was never reported, so the" +
		" server's slider stays where it was until something else writes a state")
}

func TestAVolumeCommandIsRefusedByAPlayerThatNeverOfferedOne(t *testing.T) {
	ln := listenLocal(t)
	c, _ := playingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	want := 65
	peer.writeBinary(server.sealJSON(t, typeServerComm, volumeCommand(&want)))

	ms := 1500
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&ms)))
	waitFor(t, "the delay that followed to be taken", func() bool {
		return c.heldDelay() == 1500*time.Millisecond
	})
}

func TestTheVolumePollLeavesRoomForTheReadItMakes(t *testing.T) {
	if volumeEvery <= device.VolumeReadBudget() {
		t.Errorf("the volume is polled every %v and one read may take %v, so a slow read"+
			" overlaps the next", volumeEvery, device.VolumeReadBudget())
	}
}

func muteCommand(on *bool) serverCommand {
	return serverCommand{Player: &playerCommand{Command: commandMute, Mute: on}}
}

func mutingClient(t *testing.T) (*Client, *fakeVolume) {
	t.Helper()
	c, v := volumeClient(t)
	c.Config.SetMute = v.mute
	return c, v
}

func TestHelloOffersMuteOnlyWhenThereIsAMuteToSet(t *testing.T) {
	cfg := testConfig()
	v := &fakeVolume{at: 40, ok: true}
	cfg.Level, cfg.SetVolume = v.level, v.set
	if got := cfg.playerCommands(); len(got) != 1 || got[0] != commandVolume {
		t.Errorf("a dot that cannot mute offers %v, want just %q", got, commandVolume)
	}
	cfg.SetMute = func(bool) {}
	if got := cfg.playerCommands(); len(got) != 2 || got[1] != commandMute {
		t.Errorf("client/hello offers %v, want volume and mute: aiosendspin 9.1.1 reads"+
			" both out of the hello support object", got)
	}
}

func TestTheStateCarriesTheMuteBesideTheLevel(t *testing.T) {
	ln := listenLocal(t)
	c, v := mutingClient(t)
	v.off = true
	serveOn(t, c, ln)
	_, _, state := bringUp(t, c, ln)

	if state.Player == nil || state.Player.Muted == nil {
		t.Fatal("client/state carries no mute while this player offers the command")
	}
	if !*state.Player.Muted {
		t.Error("a muted player reported itself unmuted")
	}
}

func TestAMuteAServerSetsReachesTheDevice(t *testing.T) {
	ln := listenLocal(t)
	c, v := mutingClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	on := true
	peer.writeBinary(server.sealJSON(t, typeServerComm, muteCommand(&on)))
	waitFor(t, "the mute to reach the device", func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.off
	})
}

func TestAMuteCommandIsRefusedByAPlayerThatNeverOfferedOne(t *testing.T) {
	ln := listenLocal(t)
	c, _ := volumeClient(t)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	on := true
	peer.writeBinary(server.sealJSON(t, typeServerComm, muteCommand(&on)))

	ms := 1500
	peer.writeBinary(server.sealJSON(t, typeServerComm, delayCommand(&ms)))
	waitFor(t, "the delay that followed to be taken", func() bool {
		return c.heldDelay() == 1500*time.Millisecond
	})
}
