package sendspin

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bboe/overdub/internal/audio"
)

func roles(names ...string) *[]string { return &names }

func testConfig() Config {
	return Config{
		Name:           "kitchen",
		ProductName:    "Echo Dot (2nd Generation)",
		Manufacturer:   "Amazon",
		MACAddress:     "00:00:00:00:00:01",
		UnpairedAccess: true,
		BufferCapacity: 1 << 16,
	}
}

func TestSpecWorkedExampleChoosesBetweenPairingRequiredAndUnauthorized(t *testing.T) {
	offered := offeredPairMethods()

	d := decideActivate(categorySentinel, false, offered, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}, nil)
	if d.Goodbye != goodbyePairingRequired {
		t.Errorf("playback on a sentinel with unpaired access off gave %q, want %q",
			d.Goodbye, goodbyePairingRequired)
	}

	d = decideActivate(categorySentinel, false, offered, serverActivate{
		Activities:  []string{activityPairing},
		ActiveRoles: roles(rolePlayerV1),
		Pairing:     &activatePairing{Method: methodPairingPSK},
	}, nil)
	if d.Goodbye != goodbyeUnauthorized {
		t.Errorf("roles on a pairing connection gave %q, want %q",
			d.Goodbye, goodbyeUnauthorized)
	}
}

func TestActivitySetsAllowedPerMatchedPSK(t *testing.T) {
	for _, c := range []struct {
		name       string
		matched    category
		unpaired   bool
		activities []string
		want       bool
	}{
		{"long term, playback", categoryLongTerm, false, []string{activityPlayback}, true},
		{"long term, empty", categoryLongTerm, false, nil, true},
		{"long term, pairing", categoryLongTerm, false, []string{activityPairing}, false},
		{"pairing psk, pairing", categoryPairing, false, []string{activityPairing}, true},
		{"pairing psk, playback", categoryPairing, false, []string{activityPlayback}, false},
		{"sentinel, pairing", categorySentinel, false, []string{activityPairing}, true},
		{"sentinel, playback, unpaired off", categorySentinel, false, []string{activityPlayback}, false},
		{"sentinel, playback, unpaired on", categorySentinel, true, []string{activityPlayback}, true},
		{"both at once", categoryLongTerm, true, []string{activityPlayback, activityPairing}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := activitiesAllowed(c.matched, c.unpaired, c.activities); got != c.want {
				t.Errorf("activitiesAllowed = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAPairingPSKConnectionIsNeverPlaybackCapable(t *testing.T) {
	for _, activities := range [][]string{nil, {activityPairing}} {
		if playbackCapable(categoryPairing, true, activities) {
			t.Errorf("a pairing-psk connection reported playback-capable for %v", activities)
		}
	}
}

func TestALongTermConnectionCarriesRolesWithoutPlaybackDeclared(t *testing.T) {
	d := decideActivate(categoryLongTerm, false, offeredPairMethods(), serverActivate{
		Activities:  []string{},
		ActiveRoles: roles(rolePlayerV1),
	}, nil)
	if !d.OK() {
		t.Fatalf("refused: goodbye=%q abort=%q", d.Goodbye, d.PairAbort)
	}
	if !slices.Contains(d.Roles, rolePlayerV1) {
		t.Errorf("roles = %v, want %s", d.Roles, rolePlayerV1)
	}
}

func TestPersistedRolesBecomeEmptyWhenTheConnectionStopsBeingPlaybackCapable(t *testing.T) {
	d := decideActivate(categorySentinel, false, offeredPairMethods(), serverActivate{
		Activities: []string{},
	}, []string{rolePlayerV1})
	if d.Goodbye != "" || d.PairAbort != "" {
		t.Fatalf("goodbye = %q, abort = %q, want the roles treated as empty instead",
			d.Goodbye, d.PairAbort)
	}
	if len(d.Roles) != 0 {
		t.Errorf("roles = %v, want none", d.Roles)
	}
}

func TestExplicitRolesOnANonCapableConnectionAreRefused(t *testing.T) {
	d := decideActivate(categoryLongTerm, false, offeredPairMethods(), serverActivate{
		Activities:  []string{activityPairing},
		ActiveRoles: roles(rolePlayerV1),
		Pairing:     &activatePairing{Method: methodPairingPSK},
	}, nil)
	if d.Goodbye != goodbyeUnauthorized {
		t.Errorf("goodbye = %q, want %q", d.Goodbye, goodbyeUnauthorized)
	}
}

func TestPairingMethodMustMatchTheMatchedPSK(t *testing.T) {
	offered := map[string]pairMethod{methodPairingPSK: {Locations: []string{"operator"}}}
	for _, c := range []struct {
		name    string
		matched category
		method  string
		want    string
	}{
		{"pairing psk names pairing_psk", categoryPairing, methodPairingPSK, ""},
		{"sentinel names pairing_psk", categorySentinel, methodPairingPSK, abortMethodNotSupported},
		{"pairing psk names a code method", categoryPairing, "static_pairing_code", abortMethodNotSupported},
		{"sentinel names a method we do not offer", categorySentinel, "dynamic_pairing_code", abortMethodNotSupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := decideActivate(c.matched, true, offered, serverActivate{
				Activities: []string{activityPairing},
				Pairing:    &activatePairing{Method: c.method},
			}, nil)
			if d.PairAbort != c.want {
				t.Errorf("abort = %q, want %q", d.PairAbort, c.want)
			}
		})
	}
}

func TestOfferingNoPairingMethodRefusesEveryPairingActivation(t *testing.T) {
	for _, method := range []string{methodPairingPSK, "static_pairing_code", "dynamic_pairing_code"} {
		d := decideActivate(categoryPairing, true, offeredPairMethods(), serverActivate{
			Activities: []string{activityPairing},
			Pairing:    &activatePairing{Method: method},
		}, nil)
		if d.PairAbort != abortMethodNotSupported {
			t.Errorf("%s: abort = %q, want %q", method, d.PairAbort, abortMethodNotSupported)
		}
	}
}

func TestPairingActivityWithNoPairingObjectAborts(t *testing.T) {
	d := decideActivate(categoryPairing, true, offeredPairMethods(), serverActivate{
		Activities: []string{activityPairing},
	}, nil)
	if d.PairAbort != abortMethodNotSupported {
		t.Errorf("abort = %q, want %q", d.PairAbort, abortMethodNotSupported)
	}
}

func TestHelloDeclaresPlayerAndTheFormatWeCanPlay(t *testing.T) {
	h := testConfig().hello()
	if !slices.Contains(h.SupportedRoles, rolePlayerV1) {
		t.Errorf("supported_roles = %v", h.SupportedRoles)
	}
	if h.PlayerSupport == nil {
		t.Fatal("player@v1 is listed with no player@v1_support object")
	}
	var codecs []string
	for _, f := range h.PlayerSupport.SupportedFormats {
		if f.SampleRate != StreamRate || f.Channels != StreamChannels || f.BitDepth != StreamBitDepth {
			t.Errorf("offered format = %+v", f)
		}
		codecs = append(codecs, f.Codec)
	}
	if !slices.Equal(codecs, []string{codecFLAC, codecPCM}) {
		t.Errorf("supported_formats offers %v, want flac first for the bandwidth and pcm"+
			" after it, which every server must be able to send", codecs)
	}
	if len(h.SupportedPairMethods) != 0 {
		t.Error("no pairing method is implemented yet, so none may be advertised")
	}
	if !h.UnpairedAccess.Enabled {
		t.Error("unpaired_access did not carry the configured value")
	}
}

func TestHelloMatchesTheChimeFormat(t *testing.T) {
	for _, f := range testConfig().hello().PlayerSupport.SupportedFormats {
		if f.SampleRate != audio.ChimeRate || f.Channels != audio.ChimeChannels {
			t.Errorf("%s is %d Hz / %d channel(s) and the chime is %d Hz / %d; they share"+
				" one player, so a mismatch is silence or a chime at the wrong pitch",
				f.Codec, f.SampleRate, f.Channels, audio.ChimeRate, audio.ChimeChannels)
		}
	}
}

func TestGreetAnswersServerHello(t *testing.T) {
	session, server, peer := pairedSession(t)
	go peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	type result struct {
		name string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		name, err := session.Greet(testConfig())
		done <- result{name, err}
	}()
	_, sealed := peer.read()
	got := <-done
	if got.err != nil {
		t.Fatalf("Greet: %v", got.err)
	}
	if got.name != "music assistant" {
		t.Errorf("server name = %q", got.name)
	}
	kind, body := server.open(t, sealed)
	if kind != msgJSON {
		t.Fatalf("type = %#x", kind)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if env.Type != typeClientHello {
		t.Fatalf("type = %q, want %s", env.Type, typeClientHello)
	}
	if !strings.Contains(string(env.Payload), rolePlayerV1) {
		t.Errorf("client/hello does not list %s: %s", rolePlayerV1, env.Payload)
	}
}

func TestActivateTreatsAFirstMessageWithNoRolesAsEmpty(t *testing.T) {
	s := &Session{matched: categoryLongTerm, unpaired: false, offered: offeredPairMethods()}
	got, err := s.Activate(json.RawMessage(`{"activities":["playback"]}`))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("roles = %v, want none", got)
	}
}

func TestActivateKeepsRolesWhenALaterMessageOmitsThem(t *testing.T) {
	s := &Session{matched: categoryLongTerm, unpaired: false, offered: offeredPairMethods()}
	if _, err := s.Activate(json.RawMessage(
		`{"activities":["playback"],"active_roles":["player@v1"]}`)); err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	got, err := s.Activate(json.RawMessage(`{"activities":[]}`))
	if err != nil {
		t.Fatalf("second Activate: %v", err)
	}
	if !slices.Contains(got, rolePlayerV1) {
		t.Errorf("roles = %v, want %s kept across an activation that omits them", got, rolePlayerV1)
	}
}

func TestOnlyDeclaredRolesBecomeActive(t *testing.T) {
	d := decideActivate(categorySentinel, true, offeredPairMethods(), serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1, "display@v1", "source@v1"),
	}, nil)
	if !d.OK() {
		t.Fatalf("refused: goodbye=%q abort=%q", d.Goodbye, d.PairAbort)
	}
	if !slices.Equal(d.Roles, []string{rolePlayerV1}) {
		t.Errorf("roles = %v, want only %s", d.Roles, rolePlayerV1)
	}
}

func TestARoleWeNeverDeclaredGrantsNothing(t *testing.T) {
	d := decideActivate(categorySentinel, true, offeredPairMethods(), serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles("display@v1"),
	}, nil)
	if !d.OK() {
		t.Fatalf("refused: goodbye=%q abort=%q", d.Goodbye, d.PairAbort)
	}
	if len(d.Roles) != 0 {
		t.Errorf("roles = %v, want none", d.Roles)
	}
}

func TestHelloAdvertisesTheRolesTheAdmissionRulesAccept(t *testing.T) {
	h := testConfig().hello()
	if !slices.Equal(h.SupportedRoles, supportedRoles()) {
		t.Errorf("client/hello lists %v but the admission rules accept %v",
			h.SupportedRoles, supportedRoles())
	}
}

func TestARoleRepeatedIsKeptOnce(t *testing.T) {
	repeated := make([]string, 500)
	for i := range repeated {
		repeated[i] = rolePlayerV1
	}
	got := keepSupported(repeated)
	if len(got) != 1 {
		t.Errorf("kept %d copies of one role, want 1: a server sets how long every line and"+
			" every state message naming them becomes", len(got))
	}
}

func TestHelloCarriesTheCommandsAServerRefusesUsWithout(t *testing.T) {
	h := testConfig().hello()
	if h.PlayerSupport == nil {
		t.Fatalf("client/hello declares no %s support", rolePlayerV1)
	}
	if h.PlayerSupport.SupportedCommands == nil {
		t.Error("player@v1_support carries no supported_commands; aiosendspin 9.1.1 refuses" +
			" client/hello outright without it, though the spec puts it only in client/state")
	}
}

func TestClientHelloIsExactlyThisOnTheWire(t *testing.T) {
	got, err := marshalEnvelope(typeClientHello, testConfig().hello())
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	const want = `{"type":"client/hello","payload":{"name":"kitchen",` +
		`"device_info":{"product_name":"Echo Dot (2nd Generation)","manufacturer":"Amazon",` +
		`"mac_address":"00:00:00:00:00:01"},` +
		`"supported_roles":["player@v1"],` +
		`"player@v1_support":{"supported_formats":[{"codec":"flac","channels":1,` +
		`"sample_rate":48000,"bit_depth":16},{"codec":"pcm","channels":1,` +
		`"sample_rate":48000,"bit_depth":16}],"buffer_capacity":65536,` +
		`"supported_commands":[]},"unpaired_access":{"enabled":true}}}`
	if string(got) != want {
		t.Errorf("client/hello is\n%s\nwant\n%s", got, want)
	}
}

func TestTheDeclaredNamesAreFixed(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"player role", rolePlayerV1, "player@v1"},
		{"playback activity", activityPlayback, "playback"},
		{"pairing activity", activityPairing, "pairing"},
		{"pairing psk method", methodPairingPSK, "pairing_psk"},
		{"client/hello", typeClientHello, "client/hello"},
		{"server/hello", typeServerHello, "server/hello"},
		{"server/activate", typeServerActivate, "server/activate"},
		{"client/goodbye", typeClientGoodbye, "client/goodbye"},
		{"pair/abort", typePairAbort, "pair/abort"},
		{"concurrent", goodbyeConcurrent, "concurrent_attempt"},
		{"unpaired", goodbyeUnpaired, "unpaired"},
		{"pairing required", goodbyePairingRequired, "pairing_required"},
		{"unauthorized", goodbyeUnauthorized, "unauthorized"},
		{"method not supported", abortMethodNotSupported, "method_not_supported"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"sample rate", StreamRate, 48000},
		{"channels", StreamChannels, 1},
		{"bit depth", StreamBitDepth, 16},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}
