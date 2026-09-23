package device

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	Prefix = "persist.overdub."

	keyMax = 31
)

func key(name string) (string, error) {
	full := Prefix + name
	if len(full) > keyMax {
		return "", fmt.Errorf("%s is %d characters, and this device refuses a property"+
			" name past %d", full, len(full), keyMax)
	}
	return full, nil
}

func Flag(name string) (on, known bool, err error) {
	full, err := key(name)
	if err != nil {
		return false, false, err
	}
	out, err := readProp(full)
	if err != nil {
		return false, false, fmt.Errorf("reading %s: %w", full, err)
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
	full, err := key(name)
	if err != nil {
		return err
	}
	if err := setProp(full, value); err != nil {
		return err
	}
	got, known, err := Flag(name)
	if err != nil {
		return err
	}
	if !known || got != on {
		return fmt.Errorf("%s did not read back as %s", full, value)
	}
	return nil
}

func Number(name string) (value int, known bool, err error) {
	full, err := key(name)
	if err != nil {
		return 0, false, err
	}
	out, err := readProp(full)
	if err != nil {
		return 0, false, fmt.Errorf("reading %s: %w", full, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, false, nil
	}
	return n, true, nil
}

func SetNumber(name string, value int) error {
	full, err := key(name)
	if err != nil {
		return err
	}
	if err := setProp(full, strconv.Itoa(value)); err != nil {
		return err
	}
	got, known, err := Number(name)
	if err != nil {
		return err
	}
	if !known || got != value {
		return fmt.Errorf("%s did not read back as %d", full, value)
	}
	return nil
}
