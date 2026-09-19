package device

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

const (
	setVolumeCall = "4"
	streamMusic   = "3"
	volumeCaller  = "overdub"
)

var (
	setVolumeTimeout   = 300 * time.Millisecond
	setVolumeWaitDelay = 100 * time.Millisecond

	setVolumeArgv = []string{"/system/bin/service", "call", "audio", setVolumeCall}

	setVolumeCommand = func(ctx context.Context, args []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, setVolumeArgv[0], args[1:]...)
		cmd.WaitDelay = setVolumeWaitDelay
		return cmd.Output()
	}
)

func SetMusicVolume(step int) error {
	ctx, cancel := context.WithTimeout(context.Background(), setVolumeTimeout)
	defer cancel()
	args := append(append([]string(nil), setVolumeArgv...),
		"i32", streamMusic, "i32", strconv.Itoa(step), "i32", "0", "s16", volumeCaller)
	out, err := setVolumeCommand(ctx, args)
	if err != nil {
		return fmt.Errorf("volume: step %d was refused: %w", step, err)
	}
	if !volumeTaken(string(out)) {
		return fmt.Errorf("volume: step %d was answered with %q", step, firstLine(out))
	}
	return nil
}

var volumeReply = regexp.MustCompile(`Parcel\(0{8}[\s)]`)

func volumeTaken(out string) bool { return volumeReply.MatchString(out) }

func firstLine(out []byte) string {
	for i, b := range out {
		if b == '\n' {
			return string(out[:i])
		}
	}
	return string(out)
}
