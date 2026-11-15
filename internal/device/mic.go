package device

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	buttond      = "com.doppler.buttond"
	micMuteState = "muteState"
	micStateOn   = 8
	micStateOff  = 9
)

var micReadTimeout = 300 * time.Millisecond

func privacyDir() string {
	for _, d := range privacyDirs {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return ""
}

func MicMuted() (muted, ok bool) {
	dir := privacyDir()
	if dir == "" {
		return false, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "state"))
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(b)) {
	case "0":
		return false, true
	case "1":
		return true, true
	}
	return false, false
}

func buttondMuted() bool {
	ctx, cancel := context.WithTimeout(context.Background(), micReadTimeout)
	defer cancel()
	out, err := lipcCommand(ctx, []string{lipcGetPath, "-i", "-q", buttond, micMuteState})
	if err != nil {
		return false
	}
	muted, ok := parseMicMute(string(out))
	return ok && muted
}

func parseMicMute(out string) (muted, ok bool) {
	switch v, err := strconv.Atoi(strings.TrimSpace(out)); {
	case err != nil:
		return false, false
	case v == micStateOn:
		return false, true
	case v == micStateOff:
		return true, true
	}
	return false, false
}

var (
	privacyDirs = []string{
		"/sys/devices/soc/10010000.keypad/amz_privacy",
		"/sys/devices/platform/gpio-privacy",
	}
	micNudgeWait = 2 * time.Second
	micAdoptWait = 15 * time.Second
	micAdoptPoll = 100 * time.Millisecond

	ErrMicButtonOnly = errors.New("only the mute button turns the microphones back on;" +
		" the privacy driver latches off and no write clears it")
)

func SetMicMute(muted bool, nudge func() error) error {
	if !muted {
		return ErrMicButtonOnly
	}
	dir := privacyDir()
	if dir == "" {
		return errors.New("no privacy driver to latch the microphones off")
	}
	if err := os.WriteFile(filepath.Join(dir, "enable"), []byte("1"), 0); err != nil {
		return fmt.Errorf("latching the microphones off: %w", err)
	}
	var nudged error
	if nudge != nil {
		nudged = nudge()
	}
	if nudged == nil && micAdopted(micNudgeWait) {
		return nil
	}
	if err := setProp("ctl.restart", "acebuttond"); err != nil {
		return fmt.Errorf("the microphones are off, and acebuttond was not told: %w", err)
	}
	if micAdopted(micAdoptWait) {
		return nil
	}
	return fmt.Errorf("the microphones are off, and acebuttond did not say so within %v", micAdoptWait)
}

func micAdopted(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if buttondMuted() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(micAdoptPoll)
	}
}
