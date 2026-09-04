package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const iptables = "/system/bin/iptables"

// AllowTCP checks and then appends, which is two calls rather than one, so two
// ports opened at once can both find their rule missing and both append. The
// chain is not ours alone either, so a duplicate is not something a later pass
// tidies up. One port needed no lock; the adb select is the second.
var chainMu sync.Mutex

var iptablesRun = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, iptables, append([]string{"-w"}, args...)...)
	// Killing the child does not close a descendant's copy of the pipe, and
	// CombinedOutput waits for EOF, so without this the deadline is not a bound.
	cmd.WaitDelay = 2 * time.Second
	return cmd.CombinedOutput()
}

func inputRule(port int) []string {
	return []string{"INPUT", "-i", WifiInterface, "-p", "tcp", "--dport", fmt.Sprint(port), "-j", "ACCEPT"}
}

func present(rule []string) (bool, error) {
	_, err := iptablesRun(append([]string{"-C"}, rule...)...)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func AllowTCP(port int) error {
	chainMu.Lock()
	defer chainMu.Unlock()

	r := inputRule(port)
	found, err := present(r)
	if err != nil {
		return fmt.Errorf("checking tcp/%d: %w", port, err)
	}
	if found {
		return nil
	}
	if out, err := iptablesRun(append([]string{"-A"}, r...)...); err != nil {
		return fmt.Errorf("opening tcp/%d: %w: %s", port, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DenyTCP removes the rule AllowTCP added. A rule that is not there is not an
// error: iptables exits 1 for a delete that matched nothing, which is the
// ordinary case after netd has rebuilt the chain, and the port is closed either
// way by the INPUT policy.
func DenyTCP(port int) error {
	chainMu.Lock()
	defer chainMu.Unlock()

	// Until it is gone rather than once: the chain is not ours alone, and two
	// opens racing a rebuild can leave two copies of the rule. One -D removes
	// one of them, so a single pass reports a closed port that is still
	// permitted. uninstall.sh loops over the API's rule for the same reason.
	// Bounded, because the exit that ends this loop is iptables reporting that
	// it matched nothing. An iptables that answered 0 for a delete that removed
	// nothing would spin here for ever holding chainMu, which stops tcp/6053
	// being re-asserted and takes the API away at netd's next rebuild. Sixteen
	// is far past any real duplicate.
	r := inputRule(port)
	for range 16 {
		_, err := iptablesRun(append([]string{"-D"}, r...)...)
		if err == nil {
			continue
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("closing tcp/%d: %w", port, err)
	}
	return fmt.Errorf("closing tcp/%d: the rule kept coming back", port)
}

func HoldTCPOpen(port int, every time.Duration) {
	var quiet bool
	for range time.Tick(every) {
		err := AllowTCP(port)
		if err != nil && !quiet {
			log.Printf("firewall: re-asserting tcp/%d failed: %v (further failures are silent)", port, err)
			quiet = true
		}
		if err == nil && quiet {
			log.Printf("firewall: tcp/%d is open again", port)
			quiet = false
		}
	}
}
