package device

import (
	"html"
	"os"
	"regexp"
	"strings"
	"time"
)

var btConfigPath = "/data/misc/bluedroid/bt_config.xml"

var btDump = newDumpsys("bluetooth_manager", 1000*time.Millisecond, 500*time.Millisecond)

func BluetoothReadBudget() time.Duration { return btDump.budget() }

func BluetoothDevice() (string, bool) {
	out, err := btDump.read()
	if err != nil {
		return "", false
	}
	address, ok := parseA2DPAddress(string(out))
	if !ok || address == "" {
		return "", ok
	}
	if name, ok := bluetoothName(btConfigPath, address); ok {
		if name = strings.TrimSpace(name); name != "" {
			return name, true
		}
	}
	return address, true
}

var btAddress = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

func parseA2DPAddress(dump string) (string, bool) {
	inProfile := false
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Profile:") {
			inProfile = trimmed == "Profile: A2dpService"
			continue
		}
		if !inProfile {
			continue
		}
		value, ok := strings.CutPrefix(trimmed, "mCurrentDevice:")
		if !ok {
			continue
		}
		address := strings.TrimSpace(value)
		if address == "null" {
			return "", true
		}
		if !btAddress.MatchString(address) {
			return "", false
		}
		return address, true
	}
	return "", false
}

var (
	btSection  = regexp.MustCompile(`Tag="[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}"`)
	btNameLine = regexp.MustCompile(`Tag="Name" Type="string">([^<]*)<`)
)

func bluetoothName(path, address string) (string, bool) {
	config, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	want := `Tag="` + strings.ToLower(address) + `"`
	found := false
	for _, line := range strings.Split(string(config), "\n") {
		if !found {
			found = strings.Contains(line, want)
			continue
		}
		if btSection.MatchString(line) {
			return "", false
		}
		if name := btNameLine.FindStringSubmatch(line); name != nil {
			return strings.ToValidUTF8(html.UnescapeString(name[1]), "\uFFFD"), true
		}
	}
	return "", false
}
