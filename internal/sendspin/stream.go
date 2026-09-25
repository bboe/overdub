package sendspin

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	codecPCM  = "pcm"
	codecFLAC = "flac"
)

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
	if !slices.Contains(streamRates(), p.SampleRate) || p.Channels != StreamChannels ||
		p.BitDepth != StreamBitDepth {
		return false
	}
	switch p.Codec {
	case codecPCM:
		return true
	case codecFLAC:
		return flacHeaderPlayable(p.CodecHeader, p.SampleRate)
	}
	return false
}

func (p *streamPlayer) String() string {
	format := fmt.Sprintf("%s %d Hz %d ch %d bit", untrustedlog.Cut(p.Codec), p.SampleRate,
		p.Channels, p.BitDepth)
	if p.Codec == codecFLAC && !flacHeaderPlayable(p.CodecHeader, p.SampleRate) {
		format += " with a codec_header this player cannot read"
	}
	return format
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
	s.flac = start.Player.Codec == codecFLAC
	s.rate = start.Player.SampleRate
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
