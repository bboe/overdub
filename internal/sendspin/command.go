package sendspin

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	commandStaticDelay = "set_static_delay"
	maxStaticDelayMS   = 5000
	keepApart          = time.Minute
	keepTries          = 3
)

type serverCommand struct {
	Player *playerCommand `json:"player,omitempty"`
}

type playerCommand struct {
	Command       string `json:"command"`
	StaticDelayMS *int   `json:"static_delay_ms,omitempty"`
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
	ms := max(0, min(asked, maxStaticDelayMS))
	return time.Duration(ms) * time.Millisecond, asked, true, nil
}
