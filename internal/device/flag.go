package device

import (
	"fmt"
	"strings"
)

const flagPrefix = "persist.overdub."

func Flag(name string) (on, known bool, err error) {
	out, err := readProp(flagPrefix + name)
	if err != nil {
		return false, false, fmt.Errorf("reading %s: %w", flagPrefix+name, err)
	}
	switch strings.TrimSpace(out) {
	case "1":
		return true, true, nil
	case "0":
		return false, true, nil
	}
	return false, false, nil
}

func SetFlag(name string, on bool) error {
	value := "0"
	if on {
		value = "1"
	}
	key := flagPrefix + name
	if err := setProp(key, value); err != nil {
		return err
	}
	got, known, err := Flag(name)
	if err != nil {
		return err
	}
	if !known || got != on {
		return fmt.Errorf("%s did not read back as %s", key, value)
	}
	return nil
}
