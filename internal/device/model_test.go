package device

import (
	"strings"
	"testing"
)

func TestTheBoardIsTheDeviceNameBeforeItsSuffix(t *testing.T) {
	was := readProp
	defer func() { readProp = was }()
	for device, want := range map[string]string{
		"biscuit_puffin": "biscuit", "donut_puffin": "donut", "biscuit": "biscuit", "": "",
	} {
		readProp = func(string) (string, error) { return device, nil }
		if got := Board(); got != want {
			t.Errorf("ro.product.device %q gave board %q, want %q", device, got, want)
		}
	}
}

func TestEveryModelNameCarriesNoDot(t *testing.T) {
	for _, board := range []string{"biscuit", "donut", "radar", "unknown", ""} {
		if name := ModelName(board); strings.Contains(name, ".") {
			t.Errorf("%q names %q; Home Assistant splits project_name on the dot and takes"+
				" [1] as the model, so a dot truncates it on the device page", board, name)
		}
	}
	for board, want := range map[string]string{
		"biscuit": "Echo Dot (2nd Generation)",
		"donut":   "Echo Dot (3rd Generation)",
		"radar":   "Echo (2nd Generation)",
	} {
		if got := ModelName(board); got != want {
			t.Errorf("%s is named %q, want %q", board, got, want)
		}
	}
}
