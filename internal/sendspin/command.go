package sendspin

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	commandOutputDelay = "set_output_delay"
	commandVolume      = "volume"
	commandMute        = "mute"

	KeepApart = time.Minute
	KeepTries = 3

	MaxOutputDelayMS = 5000
)

func HoldDelayMS(ms int) int { return max(0, min(ms, MaxOutputDelayMS)) }

type serverCommand struct {
	Player *playerCommand `json:"player,omitempty"`
}

type playerCommand struct {
	Command       string `json:"command"`
	OutputDelayMS *int   `json:"output_delay_ms,omitempty"`
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

func (s *Session) OutputDelay(payload json.RawMessage) (delay time.Duration, asked int,
	ours bool, err error) {
	var cmd serverCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return 0, 0, false, fmt.Errorf("server/command: %w", err)
	}
	if cmd.Player == nil || cmd.Player.Command != commandOutputDelay {
		return 0, 0, false, nil
	}
	if !holdsPlayer(s.roles) {
		return 0, 0, false, errors.New("server/command: set_output_delay for a role this" +
			" client does not hold")
	}
	if cmd.Player.OutputDelayMS == nil {
		return 0, 0, false, errors.New("server/command: set_output_delay names no" +
			" output_delay_ms")
	}
	asked = *cmd.Player.OutputDelayMS
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
