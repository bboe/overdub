package sendspin

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

const interopEnv = "SENDSPIN_INTEROP"

const interopWait = 90 * time.Second

// TestInteropWithTheReferenceServer runs a real aiosendspin server against this
// client. Every other handshake test here drives both sides from one reading of
// the spec, so a misreading would agree with itself; this one does not.
func TestInteropWithTheReferenceServer(t *testing.T) {
	if os.Getenv(interopEnv) == "" {
		t.Skipf("set %s=1 to run the reference-server interop test (needs uv)", interopEnv)
	}
	if _, err := exec.LookPath("uv"); err != nil {
		// Asked for and not run is the outcome to avoid: a skip here would let CI
		// report green while the one test that checks the wire against a real peer
		// never ran.
		t.Fatalf("%s is set but uv is not installed: %v", interopEnv, err)
	}

	ln := listenLocal(t)
	keys := testKeys(t)
	client := &Client{
		Config:      testConfig(),
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
		// --flag=value, not --flag value: a client_id is base64url and one in 64 begins
		// with a dash, which argparse reads as another option.
		"--url="+url, "--client-id="+keys.Identity.ClientID())
	// uv spawns python, so killing uv alone leaves the server running: the context
	// kill has to reach the whole process group.
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
}
