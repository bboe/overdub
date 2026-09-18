package device

import (
	"errors"
	"strings"
	"testing"
)

func TestAFlagReadsBackWhatWasWritten(t *testing.T) {
	store := map[string]string{}
	oldRead, oldSet := readProp, setProp
	readProp = func(name string) (string, error) { return store[name], nil }
	setProp = func(key, value string) error { store[key] = value; return nil }
	defer func() { readProp, setProp = oldRead, oldSet }()

	if _, known, _ := Flag("sendspin"); known {
		t.Error("an unset flag reported a value")
	}
	for _, want := range []bool{true, false, true} {
		if err := SetFlag("sendspin", want); err != nil {
			t.Fatalf("SetFlag(%v): %v", want, err)
		}
		got, known, _ := Flag("sendspin")
		if !known || got != want {
			t.Errorf("Flag = (%v, %v), want (%v, true)", got, known, want)
		}
	}
	if _, ok := store["persist.overdub.sendspin"]; !ok {
		t.Errorf("wrote %v, want a persist.overdub. name", store)
	}
}

func TestAFlagThatDoesNotReadBackIsAnError(t *testing.T) {
	oldRead, oldSet := readProp, setProp
	readProp = func(string) (string, error) { return "0", nil }
	setProp = func(string, string) error { return nil }
	defer func() { readProp, setProp = oldRead, oldSet }()

	if err := SetFlag("sendspin", true); err == nil {
		t.Error("a write that did not take was reported as success")
	}
}

func TestAFlagTellsAFailedReadFromAnUnsetOne(t *testing.T) {
	oldRead := readProp
	defer func() { readProp = oldRead }()

	readProp = func(string) (string, error) { return "", nil }
	if on, known, err := Flag("sendspin"); on || known || err != nil {
		t.Errorf("unset flag = (%v, %v, %v), want (false, false, nil)", on, known, err)
	}

	readProp = func(string) (string, error) { return "", errors.New("getprop timed out") }
	on, known, err := Flag("sendspin")
	if err == nil {
		t.Error("a failed read reported no error, so the caller cannot tell it from an unset flag")
	}
	if on || known {
		t.Errorf("a failed read = (%v, %v), want (false, false)", on, known)
	}
}

func TestAPropertyNameThisDeviceWillNotTakeIsRefusedHere(t *testing.T) {
	was := setProp
	defer func() { setProp = was }()
	tried := false
	setProp = func(string, string) error {
		tried = true
		return nil
	}

	long := strings.Repeat("x", keyMax-len(Prefix)+1)
	if err := SetNumber(long, 250); err == nil {
		t.Errorf("%s%s is %d characters and was accepted; setprop answers \"could not set"+
			" property\" past %d, measured on a Dot, so a setting written under a name"+
			" that long is never kept", Prefix, long, len(Prefix+long), keyMax)
	}
	if tried {
		t.Error("the name went to the device anyway, so the check reads the failure back" +
			" rather than heading it off")
	}
}

func TestAPropertyNameThatFitsIsStillWritten(t *testing.T) {
	was := setProp
	wasRead := readProp
	defer func() { setProp, readProp = was, wasRead }()
	setProp = func(string, string) error { return nil }
	readProp = func(string) (string, error) { return "250", nil }

	if err := SetNumber("sendspin_delay", 250); err != nil {
		t.Errorf("a name of %d characters was refused: %v",
			len(Prefix+"sendspin_delay"), err)
	}
}
