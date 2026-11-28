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

func loopbackRules(port int) [][]string {
	p := fmt.Sprint(port)
	return [][]string{
		{"INPUT", "-i", "lo", "-p", "tcp", "-m", "multiport", "--ports", p, "-j", "ACCEPT"},
		{"OUTPUT", "-o", "lo", "-p", "tcp", "-m", "multiport", "--ports", p, "-j", "ACCEPT"},
	}
}

func AllowLoopback(port int) error {
	chainMu.Lock()
	defer chainMu.Unlock()

	for _, r := range loopbackRules(port) {
		found, err := present(r)
		if err != nil {
			return fmt.Errorf("checking tcp/%d on lo: %w", port, err)
		}
		if found {
			continue
		}
		if out, err := iptablesRun(append([]string{"-A"}, r...)...); err != nil {
			return fmt.Errorf("opening tcp/%d on lo: %w: %s", port, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func DenyTCP(port int) error {
	chainMu.Lock()
	defer chainMu.Unlock()
	return removeAll(inputRule(port), fmt.Sprintf("closing tcp/%d", port))
}

func removeAll(r []string, doing string) error {
	for range 16 {
		_, err := iptablesRun(append([]string{"-D"}, r...)...)
		if err == nil {
			continue
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("%s: %w", doing, err)
	}
	return fmt.Errorf("%s: the rule kept coming back", doing)
}

const HybridUID = 5001

func hybridRule() []string {
	return []string{"OUTPUT", "!", "-o", "lo", "-m", "owner", "--uid-owner", fmt.Sprint(HybridUID), "-j", "REJECT"}
}

var hybridCut bool

func CutHybrid() error {
	chainMu.Lock()
	defer chainMu.Unlock()
	hybridCut = true
	return cutHybridLocked()
}

func reassertHybrid() error {
	chainMu.Lock()
	defer chainMu.Unlock()
	if !hybridCut {
		return nil
	}
	return cutHybridLocked()
}

func cutHybridLocked() error {
	r := hybridRule()
	found, err := present(r)
	if err != nil {
		return fmt.Errorf("checking the hybrid engine's cut: %w", err)
	}
	if found {
		return nil
	}
	if out, err := iptablesRun(append([]string{"-I", r[0], "1"}, r[1:]...)...); err != nil {
		return fmt.Errorf("cutting the hybrid engine off: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func RestoreHybrid() error {
	chainMu.Lock()
	defer chainMu.Unlock()
	hybridCut = false
	return removeAll(hybridRule(), "restoring the hybrid engine")
}

func HoldHybridCut(every time.Duration, done <-chan struct{}) {
	hold("the hybrid engine's cut", reassertHybrid, every, done)
}

func HoldTCPOpen(port int, every time.Duration, done <-chan struct{}) {
	hold(fmt.Sprintf("tcp/%d", port), func() error { return AllowTCP(port) }, every, done)
}

func HoldLoopbackOpen(port int, every time.Duration, done <-chan struct{}) {
	hold(fmt.Sprintf("tcp/%d on lo", port), func() error { return AllowLoopback(port) }, every, done)
}

func hold(what string, allow func() error, every time.Duration, done <-chan struct{}) {
	var quiet bool
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		select {
		case <-done:
			return
		default:
		}
		err := allow()
		if err != nil && !quiet {
			log.Printf("firewall: re-asserting %s failed: %v (further failures are silent)", what, err)
			quiet = true
		}
		if err == nil && quiet {
			log.Printf("firewall: %s holds again", what)
			quiet = false
		}
	}
}
