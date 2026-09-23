package device

import (
	"context"
	"os/exec"
	"time"
)

type dumpsys struct {
	argv    []string
	timeout time.Duration
	wait    time.Duration
	command func(context.Context) ([]byte, error)
}

func newDumpsys(service string, timeout, wait time.Duration) *dumpsys {
	return &dumpsys{
		argv:    []string{"/system/bin/dumpsys", service},
		timeout: timeout,
		wait:    wait,
	}
}

func (d *dumpsys) budget() time.Duration { return d.timeout + d.wait }

func (d *dumpsys) read() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	if d.command != nil {
		return d.command(ctx)
	}
	cmd := exec.CommandContext(ctx, d.argv[0], d.argv[1:]...)
	cmd.WaitDelay = d.wait
	return cmd.Output()
}
