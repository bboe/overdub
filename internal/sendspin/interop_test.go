package sendspin

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const interopEnv = "SENDSPIN_INTEROP"

const interopWait = 90 * time.Second

func runInterop(t *testing.T, player Player, outputRate func() int, args ...string) string {
	t.Helper()
	if os.Getenv(interopEnv) == "" {
		t.Skipf("set %s=1 to run the reference-server interop test (needs uv)", interopEnv)
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatalf("%s is set but uv is not installed: %v", interopEnv, err)
	}

	var said lockedLog
	was := log.Writer()
	log.SetOutput(&said)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	keys := testKeys(t)
	volume := &fakeVolume{at: 40, ok: true}
	cfg := testConfig()
	cfg.Level, cfg.SetVolume, cfg.SetMute = volume.level, volume.set, volume.mute
	cfg.OutputRate = outputRate
	client := &Client{
		Config:      cfg,
		Keys:        keys,
		PSKs:        PSKSet{Pairing: keys.PairingPSK},
		MinBufferMS: 500,
		Player:      player,
	}
	serveOn(t, client, ln)

	ctx, cancel := context.WithTimeout(context.Background(), interopWait)
	defer cancel()
	url := "ws://" + ln.Addr().String() + Path
	cmd := exec.CommandContext(ctx, "uv", append([]string{"run", "--quiet",
		"--with", "aiosendspin[server]==9.1.1",
		"python", "testdata/interop_server.py",
		"--url=" + url, "--client-id=" + keys.Identity.ClientID()}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	t.Logf("reference server said:\n%s", out)
	if err != nil {
		t.Fatalf("interop against aiosendspin failed: %v", err)
	}
	return said.String()
}

func TestInteropWithTheReferenceServer(t *testing.T) {
	said := runInterop(t, nil, nil)
	if !strings.Contains(said, "clock agreed with") {
		t.Errorf("the reference server answered every question and no answer was a"+
			" measurement, so the clock never converged. The daemon said:\n%s", said)
	}
}

func TestInteropStreamsFLACFromTheReferenceServer(t *testing.T) {
	player := &fakePlayer{}
	said := runInterop(t, player, func() int { return StreamRate }, "--play-seconds=2")
	if !strings.Contains(said, "started a flac 48000 Hz 2 ch 16 bit stream") {
		t.Errorf("the reference server did not pick flac, the first format offered. The"+
			" daemon said:\n%s", said)
	}
	if strings.Contains(said, "cannot read") {
		t.Errorf("the daemon refused audio from the reference server:\n%s", said)
	}
	if player.count() == 0 {
		t.Fatal("no stream reached the player")
	}
	want := pcmOf(flacStereo(2 * StreamRate))
	got := player.last().audio()
	whole := len(want) - len(want)%(flacVectorBlock*frameBytes)
	n := min(len(got), len(want))
	if n < whole || !bytes.Equal(got[:n], want[:n]) {
		t.Errorf("the player got %d bytes; the server streamed %d, and the %d in whole FLAC"+
			" blocks must arrive unchanged. The encoder holds a partial block, and"+
			" stopping the group drops it", len(got), len(want), whole)
	}
}

func TestInteropStreamsAt44kToADotThatAsks(t *testing.T) {
	player := &fakePlayer{}
	said := runInterop(t, player, func() int { return BluetoothRate }, "--play-seconds=2")
	if !strings.Contains(said, "started a flac 44100 Hz 2 ch 16 bit stream") {
		t.Errorf("the reference server did not send 44.1 kHz to a dot that asked for it."+
			" The daemon said:\n%s", said)
	}
	if got := player.openedAt(); !slices.Equal(got, []int{BluetoothRate}) {
		t.Fatalf("the player was opened at %v Hz, want once at %d", got, BluetoothRate)
	}
	want := 2 * BluetoothRate
	if got := len(player.last().audio()) / frameBytes; got > want || got < want-flacChunkFrames {
		t.Errorf("the player got %d frames for 2 seconds at %d Hz, want %d less at most"+
			" the encoder's partial block: 48 kHz audio labelled 44.1 would be about %d",
			got, BluetoothRate, want, 2*StreamRate)
	}
}
