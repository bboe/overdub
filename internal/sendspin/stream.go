package sendspin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const codecPCM = "pcm"

type streamPlayer struct {
	Codec       string `json:"codec"`
	SampleRate  int    `json:"sample_rate"`
	Channels    int    `json:"channels"`
	BitDepth    int    `json:"bit_depth"`
	CodecHeader string `json:"codec_header,omitempty"`
}

type streamStart struct {
	ServerTransmitted int64         `json:"server_transmitted"`
	Player            *streamPlayer `json:"player,omitempty"`
}

type streamRoles struct {
	ServerTransmitted int64     `json:"server_transmitted"`
	Roles             *[]string `json:"roles,omitempty"`
}

func (p *streamPlayer) playable() bool {
	return p.Codec == codecPCM && p.SampleRate == StreamRate &&
		p.Channels == StreamChannels && p.BitDepth == StreamBitDepth
}

func (p *streamPlayer) String() string {
	return fmt.Sprintf("%s %d Hz %d ch %d bit", untrustedlog.Cut(p.Codec), p.SampleRate,
		p.Channels, p.BitDepth)
}

func family(role string) string {
	name, _, _ := strings.Cut(role, "@")
	return name
}

func namesPlayer(roles *[]string) bool {
	if roles == nil {
		return true
	}
	return holdsPlayer(*roles)
}

func holdsPlayer(roles []string) bool {
	for _, r := range roles {
		if family(r) == familyPlayer {
			return true
		}
	}
	return false
}

func (s *Session) StartStream(payload json.RawMessage) (*streamPlayer, error) {
	var start streamStart
	if err := json.Unmarshal(payload, &start); err != nil {
		return nil, fmt.Errorf("stream/start: %w", err)
	}
	if start.Player == nil {
		return nil, nil
	}
	if !start.Player.playable() || !holdsPlayer(s.roles) {
		s.streaming = false
		return start.Player, nil
	}
	s.streaming = true
	return start.Player, nil
}

func (s *Session) EndStream(payload json.RawMessage) (bool, error) {
	var end streamRoles
	if err := json.Unmarshal(payload, &end); err != nil {
		return false, fmt.Errorf("stream/end: %w", err)
	}
	if !namesPlayer(end.Roles) {
		return false, nil
	}
	was := s.streaming
	s.streaming = false
	return was, nil
}

func (s *Session) ClearStream(payload json.RawMessage) (bool, error) {
	var clear streamRoles
	if err := json.Unmarshal(payload, &clear); err != nil {
		return false, fmt.Errorf("stream/clear: %w", err)
	}
	return namesPlayer(clear.Roles) && holdsPlayer(s.roles) && s.streaming, nil
}

func (s *Session) Streaming() bool { return s.streaming }
