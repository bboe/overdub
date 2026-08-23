// Package device reads facts about the Echo Dot itself.
// docs/architecture.md has the measurements.
package device

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

// The zone is found by its type rather than by its index: this Dot has eleven,
// their names are not zero-padded so they do not even sort into their own
// order, and nothing fixes which number the CPU lands on.
const cpuThermalType = "mtktscpu"

var thermalRoot = "/sys/class/thermal"

func CPUTemperature() (float32, bool) {
	zones, err := os.ReadDir(thermalRoot)
	if err != nil {
		return 0, false
	}
	for _, zone := range zones {
		if !strings.HasPrefix(zone.Name(), "thermal_zone") {
			continue
		}
		kind, err := os.ReadFile(filepath.Join(thermalRoot, zone.Name(), "type"))
		if err != nil || strings.TrimSpace(string(kind)) != cpuThermalType {
			continue
		}
		milli, err := os.ReadFile(filepath.Join(thermalRoot, zone.Name(), "temp"))
		if err != nil {
			return 0, false
		}
		return parseMilliCelsius(string(milli))
	}
	return 0, false
}

func parseMilliCelsius(raw string) (float32, bool) {
	milli, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	// A zone with nothing to report answers -127000, and one being read while
	// its driver is down can answer anything. Neither bound is a temperature a
	// powered SoC indoors can reach, so what falls outside them is not a
	// reading rather than a cold or burning Dot.
	if milli <= -40000 || milli >= 150000 {
		return 0, false
	}
	return float32(milli) / 1000, true
}

func WifiSignal() (float32, bool) {
	table, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return 0, false
	}
	return parseWifiLevel(string(table), WifiInterface)
}

func parseWifiLevel(table, iface string) (float32, bool) {
	for _, line := range strings.Split(table, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, iface+":") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 4 {
			return 0, false
		}
		level, err := strconv.Atoi(strings.TrimSuffix(fields[3], "."))
		if err != nil {
			return 0, false
		}
		if level > 127 {
			level -= 256
		}
		if level >= 0 || level <= -120 {
			return 0, false
		}
		return float32(level), true
	}
	return 0, false
}

var (
	volumeReadTimeout = 1500 * time.Millisecond
	volumeWaitDelay   = 500 * time.Millisecond

	// argv rather than a whole command, so a test drives the real one against a
	// child of its own choosing: what has to be bounded is exec's behaviour, and
	// a stub replacing this function cannot show that.
	volumeArgv = []string{"/system/bin/dumpsys", "audio"}

	volumeCommand = func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, volumeArgv[0], volumeArgv[1:]...)
		cmd.WaitDelay = volumeWaitDelay
		return cmd.Output()
	}
)

// The whole of one read: the deadline the child is killed at, plus the wait
// Output spends after that on a pipe the child may have left open. Exported as
// the sum, because the sum is what has to fit inside the caller's tick and the
// deadline alone is not it. A function rather than a value so a test that
// scales the two below is bounded by what it set them to.
func VolumeReadBudget() time.Duration { return volumeReadTimeout + volumeWaitDelay }

func MusicVolumePercent() (float32, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), volumeReadTimeout)
	defer cancel()
	out, err := volumeCommand(ctx)
	if err != nil {
		return 0, false
	}
	return parseMusicVolume(string(out))
}

func parseMusicVolume(dump string) (float32, bool) {
	inMusic := false
	max := 0
	muted, sawMute := false, false
	percent, havePercent := float32(0), false
	// The answer is settled at the end of the block rather than at the Current
	// line, because a Mute count printed after it still belongs to it.
	done := func() (float32, bool) {
		if !havePercent {
			return 0, false
		}
		if muted {
			return 0, true
		}
		return percent, true
	}
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- STREAM_") {
			if inMusic && havePercent {
				return done()
			}
			inMusic = trimmed == "- STREAM_MUSIC:"
			max = 0
			muted, sawMute = false, false
			continue
		}
		if trimmed != "" && line == strings.TrimLeft(line, " \t") {
			if inMusic && havePercent {
				return done()
			}
			inMusic = false
			continue
		}
		if !inMusic {
			continue
		}
		if strings.HasPrefix(trimmed, "Mute count:") {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "Mute count:"))); err == nil && !sawMute {
				muted, sawMute = n > 0, true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "Max:") {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "Max:"))); err == nil && n > 0 && max == 0 {
				max = n
			}
			continue
		}
		if strings.HasPrefix(trimmed, "Current:") {
			level, ok := speakerLevel(trimmed)
			if !ok {
				return 0, false
			}
			if max <= 0 {
				return 0, false
			}
			if level < 0 {
				level = 0
			}
			if level > max {
				level = max
			}
			percent, havePercent = float32(level)*100/float32(max), true
			continue
		}
	}
	if !inMusic {
		return 0, false
	}
	return done()
}

// A Current field is "<hex mask> (<name>): <level>".
func speakerLevel(current string) (int, bool) {
	for _, field := range strings.Split(current, ",") {
		field = strings.TrimSpace(field)
		mark := strings.LastIndex(field, ":")
		if mark < 0 || !strings.HasSuffix(strings.TrimSpace(field[:mark]), "(speaker)") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(field[mark+1:]))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
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
