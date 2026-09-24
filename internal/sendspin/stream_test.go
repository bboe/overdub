package sendspin

import (
	"encoding/json"
	"testing"
)

func ours() streamPlayer {
	return streamPlayer{
		Codec:      codecPCM,
		SampleRate: StreamRate,
		Channels:   StreamChannels,
		BitDepth:   StreamBitDepth,
	}
}

func startWith(t *testing.T, s *Session, p *streamPlayer) *streamPlayer {
	t.Helper()
	raw, err := json.Marshal(streamStart{ServerTransmitted: 1_000_000, Player: p})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	offered, err := s.StartStream(raw)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	return offered
}

func held() *Session { return &Session{roles: []string{rolePlayerV1}} }

func rolesRaw(t *testing.T, named *[]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(streamRoles{ServerTransmitted: 2_000_000, Roles: named})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

func TestAStreamInTheFormatThisPlayerDeclaredOpens(t *testing.T) {
	s := held()
	p := ours()
	if offered := startWith(t, s, &p); offered == nil {
		t.Fatal("stream/start carried a player object and none came back")
	}
	if !s.Streaming() {
		t.Error("the format this client advertises was refused")
	}
}

func TestAStreamInAFormatThisPlayerCannotPlayDoesNotOpen(t *testing.T) {
	for _, tc := range []struct {
		what string
		with func(*streamPlayer)
	}{
		{"a codec nothing here decodes", func(p *streamPlayer) { p.Codec = "opus" }},
		{"a sample rate the player is not open at", func(p *streamPlayer) { p.SampleRate = 44100 }},
		{"stereo against one speaker", func(p *streamPlayer) { p.Channels = 2 }},
		{"a bit depth the mixer does not sum", func(p *streamPlayer) { p.BitDepth = 24 }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			s := held()
			p := ours()
			tc.with(&p)
			if offered := startWith(t, s, &p); offered == nil {
				t.Fatal("the offer came back as nothing rather than as something refused")
			}
			if s.Streaming() {
				t.Errorf("%s was accepted, and audio in it would play as noise or at the"+
					" wrong pitch rather than fail", tc.what)
			}
		})
	}
}

func TestAStreamCarryingNothingForAPlayerIsNotOurs(t *testing.T) {
	s := held()
	if offered := startWith(t, s, nil); offered != nil {
		t.Error("a stream/start with no player object was read as one")
	}
	if s.Streaming() {
		t.Error("a stream for artwork or a visualizer opened the player's stream")
	}
}

func TestARefusedFormatDoesNotCloseAStreamThatWasPlaying(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)

	bad := ours()
	bad.Codec = "opus"
	startWith(t, s, &bad)
	if s.Streaming() {
		t.Error("the session still reports a stream it cannot play, so audio would be" +
			" taken for the old format")
	}
}

func TestEndingTheStreamClosesIt(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)

	ours, err := s.EndStream(rolesRaw(t, nil))
	if err != nil {
		t.Fatalf("EndStream: %v", err)
	}
	if !ours {
		t.Error("a stream/end naming no roles ends every stream, this one included")
	}
	if s.Streaming() {
		t.Error("the stream is still open after its end")
	}
}

func TestEndingSomebodyElsesStreamLeavesOursAlone(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)

	was, err := s.EndStream(rolesRaw(t, roles("visualizer", "artwork")))
	if err != nil {
		t.Fatalf("EndStream: %v", err)
	}
	if was {
		t.Error("a stream/end for other roles reported ending the player's stream")
	}
	if !s.Streaming() {
		t.Error("a stream/end for artwork closed the player's stream, so the audio that" +
			" follows it is dropped")
	}
}

func TestAStreamEndNamingTheRoleRatherThanTheFamilyStillEndsIt(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)

	if _, err := s.EndStream(rolesRaw(t, roles(rolePlayerV1))); err != nil {
		t.Fatalf("EndStream: %v", err)
	}
	if s.Streaming() {
		t.Errorf("%q was not read as the %q family, so a server that names the version"+
			" is ignored", rolePlayerV1, familyPlayer)
	}
}

func TestClearingIsOursWhenItNamesThePlayerOrNobody(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)
	for _, tc := range []struct {
		named *[]string
		want  bool
	}{
		{nil, true},
		{&[]string{}, false},
		{roles(familyPlayer), true},
		{roles(rolePlayerV1), true},
		{roles("visualizer"), false},
		{roles("artwork", familyPlayer), true},
	} {
		got, err := s.ClearStream(rolesRaw(t, tc.named))
		if err != nil {
			t.Fatalf("ClearStream: %v", err)
		}
		if got != tc.want {
			t.Errorf("stream/clear for %v is ours = %v, want %v", tc.named, got, tc.want)
		}
	}
}

func TestClearingDoesNotEndTheStream(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)

	if _, err := s.ClearStream(rolesRaw(t, nil)); err != nil {
		t.Fatalf("ClearStream: %v", err)
	}
	if !s.Streaming() {
		t.Error("a clear closed the stream; it drops what was buffered and the stream" +
			" carries on, which is what makes it different from an end")
	}
}

func TestGivingUpEveryRoleClosesTheStreamWithIt(t *testing.T) {
	s := &Session{matched: categorySentinel, unpaired: true, offered: offeredPairMethods()}
	if _, err := s.Activate(activateRaw(t, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	p := ours()
	startWith(t, s, &p)
	if !s.Streaming() {
		t.Fatal("the stream never opened, so nothing here is tested")
	}

	if _, err := s.Activate(activateRaw(t, serverActivate{
		Activities:  []string{},
		ActiveRoles: &[]string{},
	})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if s.Streaming() {
		t.Error("a server that gave up the player role left a stream open behind it," +
			" and audio would be scheduled for a role this client no longer holds")
	}
}

func activateRaw(t *testing.T, act serverActivate) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(act)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

func TestAStreamEndNamingNoRoleAtAllEndsNothing(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)

	was, err := s.EndStream(rolesRaw(t, &[]string{}))
	if err != nil {
		t.Fatalf("EndStream: %v", err)
	}
	if was {
		t.Error("an empty role list reported ending the player's stream")
	}
	if !s.Streaming() {
		t.Error("an empty role list was read as every role rather than as none, so a" +
			" stream the server left running was closed and its audio is dropped")
	}
}

func TestAStreamForARoleThisClientDoesNotHoldDoesNotOpen(t *testing.T) {
	s := &Session{}
	p := ours()
	if offered := startWith(t, s, &p); offered == nil {
		t.Fatal("the offer came back as nothing rather than as something refused")
	}
	if s.Streaming() {
		t.Error("a stream opened for a client holding no role, so audio would be" +
			" scheduled for a role the server never activated")
	}
}

func TestClearingForARoleThisClientDoesNotHoldIsNotOurs(t *testing.T) {
	s := held()
	p := ours()
	startWith(t, s, &p)
	s.roles = nil

	ours, err := s.ClearStream(rolesRaw(t, nil))
	if err != nil {
		t.Fatalf("ClearStream: %v", err)
	}
	if ours {
		t.Error("a clear was taken as this player's while it holds no role, so the buffer" +
			" hung off it is thrown away for a role the server never activated")
	}
}

func TestClearingBeforeAnyStreamIsNotOurs(t *testing.T) {
	s := held()

	ours, err := s.ClearStream(rolesRaw(t, nil))
	if err != nil {
		t.Fatalf("ClearStream: %v", err)
	}
	if ours {
		t.Error("a clear arriving before any stream reported something of ours to throw" +
			" away, which is a peer-driven log line spent on nothing")
	}
}

func TestTheFormatThisClientAdvertisesIsOneItAccepts(t *testing.T) {
	for _, f := range testConfig().hello().PlayerSupport.SupportedFormats {
		p := streamPlayer{
			Codec:      f.Codec,
			SampleRate: f.SampleRate,
			Channels:   f.Channels,
			BitDepth:   f.BitDepth,
		}
		if f.Codec == codecFLAC {
			p.CodecHeader = flacHeaderB64(t)
		}
		if !p.playable() {
			t.Errorf("client/hello advertises %s and stream/start refuses it, so a server"+
				" picking from supported_formats is told no for the format it was offered", &p)
		}
	}
}
