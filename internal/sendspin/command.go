package sendspin

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	commandStaticDelay = "set_static_delay"
	commandVolume      = "volume"
	commandMute        = "mute"

	// KeepApart and KeepTries are this package's flash-write policy, and the
	// Sendspin switch writes the same property under it when no client holds one.
	KeepApart = time.Minute
	KeepTries = 3

	MaxStaticDelayMS = 5000
)

func HoldDelayMS(ms int) int { return max(0, min(ms, MaxStaticDelayMS)) }

type serverCommand struct {
	Player *playerCommand `json:"player,omitempty"`
}

type playerCommand struct {
	Command       string `json:"command"`
	StaticDelayMS *int   `json:"static_delay_ms,omitempty"`
	Volume        *int   `json:"volume,omitempty"`
	Mute          *bool  `json:"mute,omitempty"`
}

func HoldVolume(percent int) int { return max(0, min(percent, 100)) }

func (s *Session) Volume(payload json.RawMessage) (percent int, ours bool, err error) {
	var cmd serverCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return 0, false, fmt.Errorf("server/command: %w", err)
	}
	if cmd.Player == nil || cmd.Player.Command != commandVolume {
		return 0, false, nil
	}
	if !holdsPlayer(s.roles) {
		return 0, false, errors.New("server/command: volume for a role this client does" +
			" not hold")
	}
	if cmd.Player.Volume == nil {
		return 0, false, errors.New("server/command: volume names no volume")
	}
	return *cmd.Player.Volume, true, nil
}

func (s *Session) StaticDelay(payload json.RawMessage) (delay time.Duration, asked int,
	ours bool, err error) {
	var cmd serverCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return 0, 0, false, fmt.Errorf("server/command: %w", err)
	}
	if cmd.Player == nil || cmd.Player.Command != commandStaticDelay {
		return 0, 0, false, nil
	}
	if !holdsPlayer(s.roles) {
		return 0, 0, false, errors.New("server/command: set_static_delay for a role this" +
			" client does not hold")
	}
	if cmd.Player.StaticDelayMS == nil {
		return 0, 0, false, errors.New("server/command: set_static_delay names no" +
			" static_delay_ms")
	}
	asked = *cmd.Player.StaticDelayMS
	return time.Duration(HoldDelayMS(asked)) * time.Millisecond, asked, true, nil
}

func (s *Session) Mute(payload json.RawMessage) (on, ours bool, err error) {
	var cmd serverCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return false, false, fmt.Errorf("server/command: %w", err)
	}
	if cmd.Player == nil || cmd.Player.Command != commandMute {
		return false, false, nil
	}
	if !holdsPlayer(s.roles) {
		return false, false, errors.New("server/command: mute for a role this client does" +
			" not hold")
	}
	if cmd.Player.Mute == nil {
		return false, false, errors.New("server/command: mute names no mute")
	}
	return *cmd.Player.Mute, true, nil
}
