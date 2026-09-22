// Package device reads facts about the Echo Dot itself, and switches the two
// things Home Assistant can change on it.
// docs/api.md has the measurements; docs/device.md has adb and the mic.
package device

import (
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const WifiInterface = "wlan0"

func Getprop(name string) string {
	value, err := readProp(name)
	if err != nil {
		return ""
	}
	return value
}

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

const cpuThermalType = "mtktscpu"

var thermalRoot = "/sys/class/thermal"

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
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.Atoi(fields[0])
		if err != nil || kb < 0 {
			return 0, false
		}
		return float32(kb) / 1024, true
	}
	return 0, false
}

func parseMilliCelsius(raw string) (float32, bool) {
	milli, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
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
	volumeReadTimeout = 1000 * time.Millisecond
	volumeWaitDelay   = 500 * time.Millisecond

	volumeArgv = []string{"/system/bin/dumpsys", "audio"}

	volumeCommand = func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, volumeArgv[0], volumeArgv[1:]...)
		cmd.WaitDelay = volumeWaitDelay
		return cmd.Output()
	}
)

func VolumeReadBudget() time.Duration { return volumeReadTimeout + volumeWaitDelay }

type MusicVolume struct {
	Max           int
	Muted         bool
	Speaker       float32
	SpeakerStep   int
	SpeakerOK     bool
	Jack          float32
	JackStep      int
	JackOK        bool
	Bluetooth     float32
	BluetoothStep int
	BluetoothOK   bool
}

func (v MusicVolume) sawRoute() bool { return v.SpeakerOK || v.JackOK || v.BluetoothOK }

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
	done := func() MusicVolume {
		if !found.sawRoute() {
			return MusicVolume{}
		}
		found.Muted = muted
		if muted {
			if found.SpeakerOK {
				found.Speaker = 0
			}
			if found.JackOK {
				found.Jack = 0
			}
			if found.BluetoothOK {
				found.Bluetooth = 0
			}
		}
		return found
	}
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- STREAM_") {
			if inMusic && found.sawRoute() {
				return done()
			}
			inMusic = trimmed == "- STREAM_MUSIC:"
			max = 0
			muted, sawMute = false, false
			continue
		}
		if trimmed != "" && line == strings.TrimLeft(line, " \t") {
			if inMusic && found.sawRoute() {
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
			if max <= 0 {
				return MusicVolume{}
			}
			found.Max = max
			found.SpeakerStep, found.SpeakerOK = deviceStep(trimmed, max, "speaker")
			found.Speaker = stepPercent(found.SpeakerStep, max)
			found.JackStep, found.JackOK = deviceStep(trimmed, max, "headset")
			if !found.JackOK {
				found.JackStep, found.JackOK = deviceStep(trimmed, max, "headphone")
			}
			found.Jack = stepPercent(found.JackStep, max)
			found.BluetoothStep, found.BluetoothOK = deviceStep(trimmed, max, "bt_a2dp")
			if !found.BluetoothOK {
				found.BluetoothStep, found.BluetoothOK = deviceStep(trimmed, max, "bt_a2dp_hp")
			}
			if !found.BluetoothOK {
				found.BluetoothStep, found.BluetoothOK = deviceStep(trimmed, max, "bt_a2dp_spk")
			}
			found.Bluetooth = stepPercent(found.BluetoothStep, max)
			continue
		}
	}
	if !inMusic {
		return MusicVolume{}
	}
	return done()
}

func deviceStep(current string, max int, name string) (int, bool) {
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
	return level, true
}

func stepPercent(step, max int) float32 { return float32(step) * 100 / float32(max) }

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

func HasIPv4(iface string) bool {
	nic, err := net.InterfaceByName(iface)
	if err != nil {
		return false
	}
	addrs, err := nic.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				return true
			}
		}
	}
	return false
}

func WaitForIPv4(iface string, limit time.Duration, stop <-chan struct{}) bool {
	deadline := time.Now().Add(limit)
	for {
		if HasIPv4(iface) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-stop:
			return false
		case <-time.After(time.Second):
		}
	}
}
