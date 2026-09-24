package sendspin

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	typeServerHello    = "server/hello"
	typeClientHello    = "client/hello"
	typeServerActivate = "server/activate"
	typeClientGoodbye  = "client/goodbye"
	typePairAbort      = "pair/abort"

	rolePlayerV1 = "player@v1"
	familyPlayer = "player"

	methodPairingPSK = "pairing_psk"

	activityPlayback = "playback"
	activityPairing  = "pairing"

	goodbyeShutdown         = "shutdown"
	goodbyeUnauthorized     = "unauthorized"
	goodbyePairingRequired  = "pairing_required"
	goodbyeConcurrent       = "concurrent_attempt"
	goodbyeUnpaired         = "unpaired"
	abortMethodNotSupported = "method_not_supported"
)

type deviceInfo struct {
	ProductName  string `json:"product_name,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	MACAddress   string `json:"mac_address,omitempty"`
}

type audioFormat struct {
	Codec      string `json:"codec"`
	Channels   int    `json:"channels"`
	SampleRate int    `json:"sample_rate"`
	BitDepth   int    `json:"bit_depth"`
}

type playerSupport struct {
	SupportedFormats  []audioFormat `json:"supported_formats"`
	BufferCapacity    int           `json:"buffer_capacity"`
	SupportedCommands []string      `json:"supported_commands"`
}

type pairMethod struct {
	Locations []string `json:"locations,omitempty"`
}

type unpairedAccess struct {
	Enabled bool `json:"enabled"`
}

type clientHello struct {
	Name                 string                `json:"name"`
	DeviceInfo           *deviceInfo           `json:"device_info,omitempty"`
	SupportedRoles       []string              `json:"supported_roles"`
	PlayerSupport        *playerSupport        `json:"player@v1_support,omitempty"`
	SupportedPairMethods map[string]pairMethod `json:"supported_pair_methods,omitempty"`
	UnpairedAccess       unpairedAccess        `json:"unpaired_access"`
}

type serverHello struct {
	Name string `json:"name"`
}

type activatePairing struct {
	Method string `json:"method"`
}

type serverActivate struct {
	Activities  []string         `json:"activities"`
	ActiveRoles *[]string        `json:"active_roles,omitempty"`
	Pairing     *activatePairing `json:"pairing,omitempty"`
}

type goodbye struct {
	Reason string `json:"reason"`
}

type pairAbort struct {
	Reason string `json:"reason"`
}

func allowedActivitySets(matched category, unpaired bool) [][]string {
	switch matched {
	case categoryLongTerm:
		return [][]string{{}, {activityPlayback}}
	case categoryPairing:
		return [][]string{{}, {activityPairing}}
	case categorySentinel:
		sets := [][]string{{}, {activityPairing}}
		if unpaired {
			sets = append(sets, []string{activityPlayback})
		}
		return sets
	}
	return [][]string{{}}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func activitiesAllowed(matched category, unpaired bool, activities []string) bool {
	for _, set := range allowedActivitySets(matched, unpaired) {
		if sameSet(set, activities) {
			return true
		}
	}
	return false
}

func playbackCapable(matched category, unpaired bool, activities []string) bool {
	with := slices.Clone(activities)
	if !slices.Contains(with, activityPlayback) {
		with = append(with, activityPlayback)
	}
	return activitiesAllowed(matched, unpaired, with)
}

type decision struct {
	Goodbye   string
	PairAbort string
	Roles     []string
}

func (d decision) OK() bool { return d.Goodbye == "" && d.PairAbort == "" }

func decideActivate(matched category, unpaired bool, offered map[string]pairMethod,
	act serverActivate, persisted []string) decision {

	explicit := act.ActiveRoles != nil
	roles := persisted
	if explicit {
		roles = keepSupported(*act.ActiveRoles)
	}

	if !activitiesAllowed(matched, unpaired, act.Activities) {
		if matched == categorySentinel && !unpaired &&
			admissible(matched, true, offered, act, persisted) {
			return decision{Goodbye: goodbyePairingRequired}
		}
		return decision{Goodbye: goodbyeUnauthorized}
	}

	capable := playbackCapable(matched, unpaired, act.Activities)
	if !capable && len(roles) > 0 {
		if explicit {
			if matched == categorySentinel && !unpaired &&
				admissible(matched, true, offered, act, persisted) {
				return decision{Goodbye: goodbyePairingRequired}
			}
			return decision{Goodbye: goodbyeUnauthorized}
		}
		roles = nil
	}

	if !pairingMethodOK(matched, offered, act) {
		return decision{PairAbort: abortMethodNotSupported}
	}

	return decision{Roles: roles}
}

func keepSupported(roles []string) []string {
	var out []string
	for _, r := range roles {
		if slices.Contains(supportedRoles(), r) && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func admissible(matched category, unpaired bool, offered map[string]pairMethod,
	act serverActivate, persisted []string) bool {

	if !activitiesAllowed(matched, unpaired, act.Activities) {
		return false
	}
	roles := persisted
	if act.ActiveRoles != nil {
		roles = keepSupported(*act.ActiveRoles)
	}
	if !playbackCapable(matched, unpaired, act.Activities) &&
		act.ActiveRoles != nil && len(roles) > 0 {
		return false
	}
	return pairingMethodOK(matched, offered, act)
}

func pairingMethodOK(matched category, offered map[string]pairMethod, act serverActivate) bool {
	if !slices.Contains(act.Activities, activityPairing) {
		return true
	}
	if act.Pairing == nil {
		return false
	}
	if _, ok := offered[act.Pairing.Method]; !ok {
		return false
	}
	return (act.Pairing.Method == methodPairingPSK) == (matched == categoryPairing)
}

type Config struct {
	Name           string
	ProductName    string
	Manufacturer   string
	MACAddress     string
	UnpairedAccess bool
	BufferCapacity int
	Level          func() (percent int, muted, ok bool)
	SetVolume      func(percent int)
	SetMute        func(on bool)
}

func (c Config) setsVolume() bool { return c.Level != nil && c.SetVolume != nil }

func (c Config) setsMute() bool { return c.Level != nil && c.SetMute != nil }

func (c Config) playerCommands() []string {
	out := []string{}
	if c.setsVolume() {
		out = append(out, commandVolume)
	}
	if c.setsMute() {
		out = append(out, commandMute)
	}
	return out
}

func offeredPairMethods() map[string]pairMethod { return map[string]pairMethod{} }

func supportedRoles() []string { return []string{rolePlayerV1} }

func (c Config) hello() clientHello {
	return clientHello{
		Name: c.Name,
		DeviceInfo: &deviceInfo{
			ProductName:  c.ProductName,
			Manufacturer: c.Manufacturer,
			MACAddress:   c.MACAddress,
		},
		SupportedRoles:       supportedRoles(),
		SupportedPairMethods: offeredPairMethods(),
		PlayerSupport: &playerSupport{
			SupportedFormats: []audioFormat{{
				Codec:      codecFLAC,
				Channels:   StreamChannels,
				SampleRate: StreamRate,
				BitDepth:   StreamBitDepth,
			}, {
				Codec:      codecPCM,
				Channels:   StreamChannels,
				SampleRate: StreamRate,
				BitDepth:   StreamBitDepth,
			}},
			BufferCapacity:    c.BufferCapacity,
			SupportedCommands: c.playerCommands(),
		},
		UnpairedAccess: unpairedAccess{Enabled: c.UnpairedAccess},
	}
}

func (s *Session) Greet(cfg Config) (string, error) {
	kind, payload, err := s.ReadEnvelope()
	if err != nil {
		return "", fmt.Errorf("waiting for %s: %w", typeServerHello, err)
	}
	if kind != typeServerHello {
		return "", fmt.Errorf("%w: wanted %s, got %q", errHandshake, typeServerHello, untrustedlog.Cut(kind))
	}
	var sh serverHello
	if err := json.Unmarshal(payload, &sh); err != nil {
		return "", fmt.Errorf("%w: server/hello: %w", errHandshake, err)
	}
	s.offered = offeredPairMethods()
	s.unpaired = cfg.UnpairedAccess
	if err := s.WriteJSON(typeClientHello, cfg.hello()); err != nil {
		return "", err
	}
	return sh.Name, nil
}

func (s *Session) Activate(payload json.RawMessage) ([]string, error) {
	var act serverActivate
	if err := json.Unmarshal(payload, &act); err != nil {
		return nil, fmt.Errorf("%w: server/activate: %w", errHandshake, err)
	}
	d := decideActivate(s.matched, s.unpaired, s.offered, act, s.roles)
	switch {
	case d.Goodbye != "":
		_ = s.WriteJSON(typeClientGoodbye, goodbye{Reason: d.Goodbye})
		return nil, fmt.Errorf("%w: refused server/activate: %s", errHandshake, d.Goodbye)
	case d.PairAbort != "":
		if err := s.WriteJSON(typePairAbort, pairAbort{Reason: d.PairAbort}); err != nil {
			return nil, err
		}
		return s.roles, nil
	}
	s.roles = d.Roles
	if !holdsPlayer(s.roles) {
		s.streaming = false
	}
	return s.roles, nil
}

func (s *Session) closeWS(code int, reason string) error {
	s.writing.Lock()
	defer s.writing.Unlock()
	return s.ws.Close(code, reason)
}

func (s *Session) Goodbye(reason string) error {
	return s.WriteJSON(typeClientGoodbye, goodbye{Reason: reason})
}

const (
	StreamRate     = 48000
	StreamChannels = 2
	StreamBitDepth = 16
)
