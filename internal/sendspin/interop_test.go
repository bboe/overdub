package sendspin

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const interopEnv = "SENDSPIN_INTEROP"

const interopWait = 90 * time.Second

func TestInteropWithTheReferenceServer(t *testing.T) {
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
	cfg.Volume, cfg.SetVolume = volume.read, volume.set
	client := &Client{
		Config:      cfg,
		Keys:        keys,
		PSKs:        PSKSet{Pairing: keys.PairingPSK},
		MinBufferMS: 500,
	}
	serveOn(t, client, ln)

	ctx, cancel := context.WithTimeout(context.Background(), interopWait)
	defer cancel()
	url := "ws://" + ln.Addr().String() + Path
	cmd := exec.CommandContext(ctx, "uv", "run", "--quiet",
		"--with", "aiosendspin[server]==9.1.1",
		"python", "testdata/interop_server.py",
		"--url="+url, "--client-id="+keys.Identity.ClientID())
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
	if !strings.Contains(said.String(), "clock agreed with") {
		t.Errorf("the reference server answered every question and no answer was a"+
			" measurement, so the clock never converged. The daemon said:\n%s",
			said.String())
	}
}
