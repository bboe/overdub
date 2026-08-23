// Package device reads facts about the Echo Dot itself.
// docs/architecture.md has the measurements.
package device

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const WifiInterface = "wlan0"

// MACAddress reports the interface's address, or "" if it has none yet: the
// node exists before the driver is up, and reads as all zeroes until it is.
func MACAddress(iface string) string {
	b, err := os.ReadFile("/sys/class/net/" + iface + "/address")
	if err != nil {
		return ""
	}
	mac := strings.ToUpper(strings.TrimSpace(string(b)))
	if mac == zeroMAC {
		return ""
	}
	return mac
}

func UptimeSeconds() (float32, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	return parseUptime(string(b))
}

func parseUptime(line string) (float32, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 32)
	if err != nil {
		return 0, false
	}
	return float32(v), true
}

const zeroMAC = "00:00:00:00:00:00"

func WaitForMAC(iface string, limit time.Duration) string {
	deadline := time.Now().Add(limit)
	logged := false
	for {
		if mac := MACAddress(iface); mac != "" {
			if logged {
				log.Printf("%s appeared", iface)
			}
			return mac
		}
		if time.Now().After(deadline) {
			return ""
		}
		if !logged {
			log.Printf("waiting for %s to appear", iface)
			logged = true
		}
		time.Sleep(time.Second)
	}
}
