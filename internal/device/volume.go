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
	setMuteCall   = "8"
	streamMusic   = "3"
	volumeCaller  = "overdub"
)

var (
	setVolumeTimeout   = 300 * time.Millisecond
	setVolumeWaitDelay = 100 * time.Millisecond

	setVolumeArgv = []string{"/system/bin/service", "call", "audio", setVolumeCall}
	muteArgv      = []string{"/system/bin/service", "call", "audio", setMuteCall}

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

func SetMusicMute(on bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), setVolumeTimeout)
	defer cancel()
	state := "0"
	if on {
		state = "1"
	}
	args := append(append([]string(nil), muteArgv...), "i32", streamMusic, "i32", state)
	out, err := setVolumeCommand(ctx, args)
	if err != nil {
		return fmt.Errorf("volume: the mute was refused: %w", err)
	}
	if !volumeTaken(string(out)) {
		return fmt.Errorf("volume: the mute was answered with %q", firstLine(out))
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
