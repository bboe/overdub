package device

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

const micMuteCall = "18"

var (
	micReadTimeout = 300 * time.Millisecond
	micWaitDelay   = 100 * time.Millisecond

	micArgv = []string{"/system/bin/service", "call", "media.audio_flinger", micMuteCall}

	micCommand = func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, micArgv[0], micArgv[1:]...)
		cmd.WaitDelay = micWaitDelay
		return cmd.Output()
	}
)

func MicReadBudget() time.Duration { return micReadTimeout + micWaitDelay }

func MicMuted() (muted, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), micReadTimeout)
	defer cancel()
	out, err := micCommand(ctx)
	if err != nil {
		return false, false
	}
	return parseMicMute(string(out))
}

var micReply = regexp.MustCompile(`Parcel\(([0-9a-fA-F]{8})`)

func parseMicMute(out string) (muted, ok bool) {
	m := micReply.FindStringSubmatch(out)
	if m == nil {
		return false, false
	}
	v, err := strconv.ParseUint(m[1], 16, 32)
	if err != nil {
		return false, false
	}
	switch v {
	case 0:
		return false, true
	case 1:
		return true, true
	}
	return false, false
}
