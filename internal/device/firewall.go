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

var chainMu sync.Mutex

var iptablesRun = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, iptables, append([]string{"-w"}, args...)...)
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

func DenyTCP(port int) error {
	chainMu.Lock()
	defer chainMu.Unlock()

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
