package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

const iptables = "/system/bin/iptables"

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
