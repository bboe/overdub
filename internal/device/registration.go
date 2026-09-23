package device

import (
	"strings"
	"time"
)

var accountDump = newDumpsys("account", 1000*time.Millisecond, 500*time.Millisecond)

func AccountReadBudget() time.Duration { return accountDump.budget() }

const (
	accountCount = "Accounts:"
	accountEntry = "Account {"
	amazonType   = "type=com.amazon.account}"
)

func AlexaRegistered() (registered, ok bool) {
	out, err := accountDump.read()
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
