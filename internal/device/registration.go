package device

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

var (
	accountReadTimeout = 1000 * time.Millisecond
	accountWaitDelay   = 500 * time.Millisecond

	accountArgv = []string{"/system/bin/dumpsys", "account"}

	accountCommand = func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, accountArgv[0], accountArgv[1:]...)
		cmd.WaitDelay = accountWaitDelay
		return cmd.Output()
	}
)

func AccountReadBudget() time.Duration { return accountReadTimeout + accountWaitDelay }

const (
	accountCount = "Accounts:"
	accountEntry = "Account {"
	amazonType   = "type=com.amazon.account}"
)

func AlexaRegistered() (registered, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), accountReadTimeout)
	defer cancel()
	out, err := accountCommand(ctx)
	if err != nil {
		return false, false
	}
	return parseRegistered(string(out))
}

func parseRegistered(dump string) (registered, ok bool) {
	counted := false
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, accountCount) {
			counted = true
			continue
		}
		if strings.HasPrefix(trimmed, accountEntry) && strings.HasSuffix(trimmed, amazonType) {
			return true, true
		}
	}
	return false, counted
}
