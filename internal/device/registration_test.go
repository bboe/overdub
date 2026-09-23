package device

import (
	"context"
	"errors"
	"testing"
)

const accountDumpRegistered = `User UserInfo{0:Owner:13}:
  Accounts: 1
    Account {name=Bryce, type=com.amazon.account}

  Active Sessions: 0

  RegisteredServicesCache: 1 services
    ServiceInfo: AuthenticatorDescription {type=com.amazon.account}, ComponentInfo{com.amazon.imp/com.amazon.dcp.sso.AccountAuthenticationService}, uid 32051
`

const accountDumpEmpty = `User UserInfo{0:Owner:13}:
  Accounts: 0

  Active Sessions: 0

  RegisteredServicesCache: 1 services
    ServiceInfo: AuthenticatorDescription {type=com.amazon.account}, ComponentInfo{com.amazon.imp/com.amazon.dcp.sso.AccountAuthenticationService}, uid 32051
`

func TestParseRegisteredReadsTheAccountList(t *testing.T) {
	for _, tt := range []struct {
		name       string
		dump       string
		registered bool
		ok         bool
	}{
		{"an account", accountDumpRegistered, true, true},
		{"no account", accountDumpEmpty, false, true},
		{"somebody else's account", "  Accounts: 1\n    Account {name=x, type=com.google}\n", false, true},
		{"a second user holds it", accountDumpEmpty + "User UserInfo{10:Guest:0}:\n  Accounts: 1\n" +
			"    Account {name=Bryce, type=com.amazon.account}\n", true, true},
		{"a dump that is not one", "Can't find service: account\n", false, false},
		{"nothing at all", "", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			registered, ok := parseRegistered(tt.dump)
			if registered != tt.registered || ok != tt.ok {
				t.Errorf("parseRegistered() = %v, %v; want %v, %v",
					registered, ok, tt.registered, tt.ok)
			}
		})
	}
}

func TestTheAuthenticatorIsNotAnAccount(t *testing.T) {
	if registered, _ := parseRegistered(accountDumpEmpty); registered {
		t.Error("the ServiceInfo line was read as an account; it names the same type on a Dot " +
			"with none")
	}
}

func TestARegistrationReadThatFailedIsNoReading(t *testing.T) {
	defer func(was dumpsys) { *accountDump = was }(*accountDump)

	accountDump.command = func(context.Context) ([]byte, error) {
		return []byte(accountDumpRegistered), errors.New("killed")
	}
	if registered, ok := AlexaRegistered(); ok || registered {
		t.Errorf("AlexaRegistered() = %v, %v; want a reading nobody took", registered, ok)
	}

	accountDump.command = func(context.Context) ([]byte, error) {
		return []byte(accountDumpRegistered), nil
	}
	if registered, ok := AlexaRegistered(); !ok || !registered {
		t.Errorf("AlexaRegistered() = %v, %v; want true, true", registered, ok)
	}
}
