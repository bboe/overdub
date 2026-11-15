package device

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const audioSink = "0000110b-0000-1000-8000-00805f9b34fb"

var (
	btConfigPath = "/data/misc/bluedroid/bt_config.conf"
	btClientArgv = []string{"/system/bin/btclient"}
	btTimeout    = time.Second
	btWait       = 500 * time.Millisecond
	btQuitAfter  = 300 * time.Millisecond
)

func BluetoothReadBudget() time.Duration { return btTimeout + btWait }

func BluetoothRouted() (connected, known bool) {
	volume.mu.Lock()
	output, ok := volume.output, volume.outputOK
	volume.mu.Unlock()
	if !ok {
		return false, false
	}
	return outputBluetooth(output)
}

func outputBluetooth(output string) (connected, known bool) {
	kind, _, ok := strings.Cut(output, "##")
	if !ok {
		return false, false
	}
	switch kind {
	case "BT":
		return true, true
	case "SPEAKER", "AUX":
		return false, true
	}
	return false, false
}

func BluetoothDevice() (string, bool) {
	connected, known := BluetoothRouted()
	if !known {
		return "", false
	}
	if !connected {
		return "", true
	}
	addresses, err := connectedDevices()
	if err != nil || len(addresses) == 0 {
		return "", false
	}
	var sections map[string]map[string]string
	if config, err := os.ReadFile(btConfigPath); err == nil {
		sections = parseBTConfig(string(config))
	}
	pick := ""
	for _, address := range addresses {
		if strings.Contains(strings.ToLower(sections[address]["Service"]), audioSink) {
			pick = address
			break
		}
	}
	if pick == "" {
		if len(addresses) > 1 {
			return "", false
		}
		pick = addresses[0]
	}
	if name := strings.TrimSpace(sections[pick]["Name"]); name != "" {
		return strings.ToValidUTF8(name, "\uFFFD"), true
	}
	return pick, true
}

var (
	btAddress = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)
	btLine    = regexp.MustCompile(`INFO\[\d+\]: (.*)$`)
	btCount   = regexp.MustCompile(`^Got (\d+) devices for getConnectedDevices$`)
)

func connectedDevices() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), btTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, btClientArgv[0], btClientArgv[1:]...)
	cmd.WaitDelay = btWait
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stop()
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	if _, err := io.WriteString(stdin, "connectedlist\n"); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(btQuitAfter):
	}
	if _, err := io.WriteString(stdin, "quit\n"); err != nil {
		return nil, err
	}
	return readConnected(stdout)
}

func readConnected(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	want := -1
	var addresses []string
	for sc.Scan() {
		line := btLine.FindStringSubmatch(sc.Text())
		if line == nil {
			continue
		}
		if want < 0 {
			if m := btCount.FindStringSubmatch(line[1]); m != nil {
				want, _ = strconv.Atoi(m[1])
			}
		} else if btAddress.MatchString(line[1]) {
			addresses = append(addresses, strings.ToLower(line[1]))
		}
		if want >= 0 && len(addresses) == want {
			return addresses, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

func parseBTConfig(config string) map[string]map[string]string {
	sections := map[string]map[string]string{}
	var current map[string]string
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "["); ok {
			name = strings.TrimSuffix(name, "]")
			current = nil
			if btAddress.MatchString(name) {
				current = map[string]string{}
				sections[strings.ToLower(name)] = current
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if current != nil && ok {
			current[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return sections
}
