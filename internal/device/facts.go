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
	"sync"
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

// The zone is looked for once and then read by path. Measured on the Dot, the
// search costs 4.6ms against 118us for the read it ends in, because it opens
// every type file in a directory holding eleven zones and fifty-four cooling
// devices. Zones do not appear or move while the kernel is up, so paying that
// per reading buys nothing.
var (
	thermalMu   sync.Mutex
	cpuZonePath string
)

func CPUTemperature() (float32, bool) {
	thermalMu.Lock()
	defer thermalMu.Unlock()
	if cpuZonePath == "" {
		cpuZonePath = findCPUZone()
		if cpuZonePath == "" {
			return 0, false
		}
	}
	milli, err := os.ReadFile(cpuZonePath)
	if err != nil {
		// The path was good once. Something changed under us, so the next
		// reading looks again rather than reporting nothing for the rest of
		// the boot.
		cpuZonePath = ""
		return 0, false
	}
	return parseMilliCelsius(string(milli))
}

func findCPUZone() string {
	zones, err := os.ReadDir(thermalRoot)
	if err != nil {
		return ""
	}
	for _, zone := range zones {
		if !strings.HasPrefix(zone.Name(), "thermal_zone") {
			continue
		}
		kind, err := os.ReadFile(filepath.Join(thermalRoot, zone.Name(), "type"))
		if err != nil || strings.TrimSpace(string(kind)) != cpuThermalType {
			continue
		}
		return filepath.Join(thermalRoot, zone.Name(), "temp")
	}
	return ""
}

// MemAvailable rather than MemFree. Measured on this Dot: 35 MiB free of 472,
// beside 123 MiB of cache the kernel would hand back on demand, which is why
// MemAvailable says 126. MemFree reads as a device about to fall over and
// MemAvailable reads as what an allocation could actually get.
func AvailableMemory() (float32, bool) {
	info, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseAvailableMemory(string(info))
}

func parseAvailableMemory(info string) (float32, bool) {
	for _, line := range strings.Split(info, "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "MemAvailable:"))
		// The unit is the kernel's to state. Every line in this file is kB
		// today, and a reading scaled by a unit we guessed at would be wrong
		// rather than absent.
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.Atoi(fields[0])
		if err != nil || kb < 0 {
			return 0, false
		}
		return float32(kb) / 1024, true
	}
	// Absent before Linux 3.14. Reconstructing it from MemFree and Cached is
	// the guessed denominator again: the kernel's own estimate accounts for
	// what it cannot reclaim, and an approximation of it is not this reading.
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

// Both levels come out of one dump, because the dump costs a fork and the two
// numbers have to describe the same moment.
type MusicVolume struct {
	Speaker   float32
	SpeakerOK bool
	Jack      float32
	JackOK    bool
}

func MusicVolumes() MusicVolume {
	ctx, cancel := context.WithTimeout(context.Background(), volumeReadTimeout)
	defer cancel()
	out, err := volumeCommand(ctx)
	if err != nil {
		return MusicVolume{}
	}
	return parseMusicVolumes(string(out))
}

func parseMusicVolumes(dump string) MusicVolume {
	inMusic := false
	max := 0
	muted, sawMute := false, false
	var found MusicVolume
	// The answer is settled at the end of the block rather than at the Current
	// line, because a Mute count printed after it still belongs to it.
	done := func() MusicVolume {
		if muted {
			// The stream is muted, so neither route is audible. The levels are
			// still what each would return to.
			if found.SpeakerOK {
				found.Speaker = 0
			}
			if found.JackOK {
				found.Jack = 0
			}
		}
		return found
	}
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- STREAM_") {
			if inMusic && (found.SpeakerOK || found.JackOK) {
				return done()
			}
			inMusic = trimmed == "- STREAM_MUSIC:"
			max = 0
			muted, sawMute = false, false
			continue
		}
		if trimmed != "" && line == strings.TrimLeft(line, " \t") {
			if inMusic && (found.SpeakerOK || found.JackOK) {
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
			// No denominator, no readings: a guessed one reports a percentage
			// that is wrong rather than absent.
			if max <= 0 {
				return MusicVolume{}
			}
			found.Speaker, found.SpeakerOK = devicePercent(trimmed, max, "speaker")
			// headset is the only one this Dot has ever used, whatever is
			// plugged in. headphone is what another build might route to.
			found.Jack, found.JackOK = devicePercent(trimmed, max, "headset")
			if !found.JackOK {
				found.Jack, found.JackOK = devicePercent(trimmed, max, "headphone")
			}
			continue
		}
	}
	if !inMusic {
		return MusicVolume{}
	}
	return done()
}

func devicePercent(current string, max int, name string) (float32, bool) {
	level, ok := deviceLevel(current, name)
	if !ok {
		return 0, false
	}
	if level < 0 {
		level = 0
	}
	if level > max {
		level = max
	}
	return float32(level) * 100 / float32(max), true
}

// 0 is nothing in the jack and 1 is something. This driver reports every plug
// as a headset whatever it is, so the value is a plug and not a device: a
// three-pole cable with no microphone pole at all reads 1, and 2, which would
// mean a plug it thinks has no microphone, has never been seen.
var jackSwitchPath = "/sys/class/switch/h2w/state"

func JackOccupied() (bool, bool) {
	state, err := os.ReadFile(jackSwitchPath)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(state)) {
	case "0":
		return false, true
	case "1", "2":
		return true, true
	}
	return false, false
}

// A Current field is "<hex mask> (<name>): <level>".
func deviceLevel(current, name string) (int, bool) {
	for _, field := range strings.Split(current, ",") {
		field = strings.TrimSpace(field)
		mark := strings.LastIndex(field, ":")
		if mark < 0 || !strings.HasSuffix(strings.TrimSpace(field[:mark]), "("+name+")") {
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
