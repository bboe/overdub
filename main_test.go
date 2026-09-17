package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"time"

	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
	"github.com/bboe/overdub/internal/mdns"
	"github.com/bboe/overdub/internal/sendspin"
	"github.com/bboe/overdub/internal/untrustedlog"
)

func TestCheckName(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "a"
	}
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{"kitchen", true},
		{"echo-dot_2", true},
		{long[:63], true},
		{long, false},
		{"-kitchen", false},
		{"kitchen-", false},
		{"Kitchen", false},
		{"kitchen.local", false},
		{"kitchen dot", false},
	} {
		if err := checkName(tt.name); (err == nil) != tt.ok {
			t.Errorf("checkName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestUninstallDeletesTheRuleTheDaemonOpens(t *testing.T) {
	script, err := os.ReadFile("deploy/uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	shape := fmt.Sprintf("-i %s -p tcp --dport $port -j ACCEPT", wifiIface)
	if !strings.Contains(string(script), shape) {
		t.Errorf("deploy/uninstall.sh deletes no rule matching %q, and the daemon adds exactly that",
			shape)
	}
	assigned := map[string]string{}
	for _, line := range strings.Split(string(script), "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			assigned[name] = value
		}
	}
	looped := map[string]bool{}
	for _, line := range strings.Split(string(script), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "for port in ") {
			continue
		}
		for _, field := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "for port in ")) {
			name := strings.Trim(field, `"$;`)
			if value, ok := assigned[name]; ok {
				looped[value] = true
			}
			looped[name] = true
		}
	}
	for _, port := range []int{apiPort, sendspin.Port} {
		if !looped[strconv.Itoa(port)] {
			t.Errorf("deploy/uninstall.sh deletes no rule for tcp/%d, which the daemon opens", port)
		}
	}
}

func TestTheScriptsUseTheKeyPathTheDaemonReads(t *testing.T) {
	for _, name := range []string{"deploy/install.sh", "deploy/uninstall.sh"} {
		script, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		named, acts := false, false
		for _, line := range strings.Split(string(script), "\n") {
			trimmed := strings.TrimSpace(line)
			inert := strings.HasPrefix(trimmed, "KEY=") ||
				strings.HasPrefix(trimmed, "#") ||
				strings.HasPrefix(trimmed, "echo ")
			if strings.Contains(line, noiseKeyPath) {
				named = true
				if !inert {
					acts = true
				}
				continue
			}
			if !inert && strings.Contains(line, "$KEY") {
				acts = true
			}
		}
		if !named {
			t.Errorf("%s never spells %q, so it acts on some other path", name, noiseKeyPath)
		}
		if !acts {
			t.Errorf("%s names %q and then never acts on it", name, noiseKeyPath)
		}
	}
}

func TestTheReadmeNamesTheKeyPathTheDaemonReads(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), noiseKeyPath) {
		t.Errorf("README.md never names %q, so its rotation instructions point elsewhere", noiseKeyPath)
	}
}

func TestTheSensorTickRespectsTheFloor(t *testing.T) {
	if sensorTick < esphome.MinSensorTick {
		t.Errorf("sensorTick is %v, under the %v floor PollSensors would raise it to", sensorTick, esphome.MinSensorTick)
	}
	floor := sensorTick
	if floor < esphome.MinSensorTick {
		floor = esphome.MinSensorTick
	}
	if budget := device.AccountReadBudget(); budget >= floor {
		t.Errorf("the registration is read every %v and one read may take %v, so a slow read "+
			"delays the tick behind it", floor, budget)
	}
}

func TestTheLiveTickIsPositiveAndBeatsTheSensorTick(t *testing.T) {
	if liveTick <= 0 {
		t.Errorf("liveTick is %v, and time.NewTicker panics on that", liveTick)
	}
	heavy := liveTick * esphome.HeavyEvery
	if heavy <= device.VolumeReadBudget() {
		t.Errorf("the expensive readings run every %v and one may take %v, so a slow read "+
			"delays the readings behind it", heavy, device.VolumeReadBudget())
	}
	stacked := device.VolumeReadBudget() + device.MicReadBudget() + device.SpeakerReadBudget()
	if stacked >= heavy {
		t.Errorf("one heavy tick can spend %v on its forks against an interval of %v, so a "+
			"device that answers slowly pushes the next tick out", stacked, heavy)
	}
	if esphome.HeavyEvery < 2 {
		t.Errorf("HeavyEvery is %d, so every tick is a heavy one and the tick buys nothing",
			esphome.HeavyEvery)
	}
	if soundEvery := liveTick * esphome.SoundEvery; soundEvery <= device.SpeakerReadBudget() {
		t.Errorf("sound is read every %v and one may take %v, so a slow read delays the "+
			"readings behind it", soundEvery, device.SpeakerReadBudget())
	}
	if esphome.SoundEvery >= esphome.HeavyEvery {
		t.Errorf("SoundEvery is %d against HeavyEvery's %d, so sound is read no oftener than "+
			"the fork it was separated from", esphome.SoundEvery, esphome.HeavyEvery)
	}
	sound := liveTick * esphome.SoundEvery
	if esphome.SoundOnDelay <= sound {
		t.Errorf("SoundOnDelay is %v against a sample every %v, so one reading turns it on",
			esphome.SoundOnDelay, sound)
	}
	if esphome.SoundOffDelay <= sound {
		t.Errorf("SoundOffDelay is %v against a sample every %v, so one reading turns it off",
			esphome.SoundOffDelay, sound)
	}
	if liveTick >= sensorTick {
		t.Errorf("liveTick is %v against a sensor tick of %v, so a poll of its own buys nothing",
			liveTick, sensorTick)
	}
}

func TestTheReadmeQuotesTheMissingKeyError(t *testing.T) {
	const said = "(deploy/install.sh generates one)"
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), said) {
		t.Errorf("serve.go no longer says %q", said)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), said) {
		t.Errorf("README.md quotes an error serve.go does not write")
	}
}

func TestServeWiresTheButtonToTheSelect(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "server.UseButton(") {
		t.Error("serve.go never calls UseButton, so action_button_mode moves nothing and says so to nobody")
	}
}

func TestEveryGestureHasItsStandardName(t *testing.T) {
	for _, tt := range []struct {
		gesture button.Gesture
		want    esphome.EventType
	}{
		{button.GesturePressEnd, "press_end"},
		{button.GestureMultiEnd, "multi_press_end"},
		{button.GestureLongStart, "long_press_start"},
		{button.GestureLongEnd, "long_press_end"},
	} {
		got, ok := pressEvent(tt.gesture)
		if !ok {
			t.Errorf("%v has no esphome name, so it would not be reported", tt.gesture)
			continue
		}
		if got != tt.want {
			t.Errorf("%v is sent as %q, want %q", tt.gesture, got, tt.want)
		}
	}
	if _, ok := pressEvent(button.Gesture(99)); ok {
		t.Error("an unrecognised gesture is given an esphome name, so it would be reported as one")
	}
}

func TestTheDocsPromiseTheHoldThresholdTheDaemonUses(t *testing.T) {
	const said = "six hundred milliseconds"
	const promised = 600 * time.Millisecond
	if holdTime != promised {
		t.Errorf("holdTime is %v and the docs promise %q; change them together", holdTime, said)
	}
	for _, path := range []string{"README.md", "docs/button.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), said) {
			t.Errorf("%s no longer says %q, which is the threshold the daemon uses", path, said)
		}
	}
}

func TestServeGivesThePressSomewhereToGo(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, said := range []string{"api.Store(server)", "server.FirePress("} {
		if !strings.Contains(string(source), said) {
			t.Errorf("serve.go never says %q, so a press reaches Home Assistant by no route", said)
		}
	}
}

func TestTheDocsPromiseTheGapTheDaemonUses(t *testing.T) {
	const said = "three hundred and fifty milliseconds"
	const promised = 350 * time.Millisecond
	if multiGap != promised {
		t.Errorf("multiGap is %v and the docs promise %q; change them together", multiGap, said)
	}
	body, err := os.ReadFile("docs/button.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), said) {
		t.Errorf("docs/button.md no longer says %q, which is the gap the daemon uses", said)
	}
}

func TestTheMultiGapIsShorterThanTheHold(t *testing.T) {
	if multiGap <= 0 {
		t.Fatalf("multiGap is %v; a run would be reported before its next press could join it", multiGap)
	}
	if multiGap >= holdTime {
		t.Errorf("multiGap is %v and holdTime is %v; the gap has to be the shorter of the two",
			multiGap, holdTime)
	}
}

func TestTheBlueprintSelectsOnTheGesturesTheDaemonSends(t *testing.T) {
	body, err := os.ReadFile("ha/dot-action-button.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []esphome.EventType{
		esphome.EventPressEnd,
		esphome.EventMultiEnd,
		esphome.EventLongPressStart,
		esphome.EventLongPressEnd,
	} {
		if !strings.Contains(string(body), "'"+string(event)+"'") {
			t.Errorf("the blueprint never selects on %q, so that gesture reaches no action", event)
		}
	}
	for _, key := range []string{"event_type", "multi_press_count", "held_ms", "device"} {
		if !strings.Contains(string(body), "trigger.event.data."+key) {
			t.Errorf("the blueprint never reads trigger.event.data.%s", key)
		}
	}
}

func TestOnlyAnInterceptedPressChimes(t *testing.T) {
	for _, tt := range []struct {
		mode button.Mode
		want bool
	}{
		{button.ModeIntercept, true},
		{button.ModeMonitor, false},
		{button.ModePassThrough, false},
	} {
		if got := chimes(138, tt.mode); got != tt.want {
			t.Errorf("an action-button press in %v chimes=%v, want %v", tt.mode, got, tt.want)
		}
	}
}

func TestAnInterceptedMuteIsSilent(t *testing.T) {
	for _, mode := range []button.Mode{button.ModeIntercept, button.ModeMonitor, button.ModePassThrough} {
		if chimes(113, mode) {
			t.Errorf("a mute press in %v chimes, which sounds like the mic was cut", mode)
		}
	}
}

func TestEveryOfferedModeParsesBackToAMode(t *testing.T) {
	for _, m := range []button.Mode{button.ModeIntercept, button.ModeMonitor, button.ModePassThrough} {
		got, ok := parseButtonMode(m.String())
		if !ok {
			t.Errorf("mode %v is named %q, which parseButtonMode does not know", m, m.String())
			continue
		}
		if got != m {
			t.Errorf("%q parsed back to %v, want %v", m.String(), got, m)
		}
	}
	if _, ok := parseButtonMode("captured"); ok {
		t.Error("a name the button has no mode for parsed anyway")
	}
}

func TestEveryWatchedKeyHasAnEntityAndEveryEntityAKey(t *testing.T) {
	s := esphome.NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)

	for code, want := range map[uint16]string{138: "action_button", 113: "mute_button"} {
		b, ok := buttons[code]
		if !ok {
			t.Errorf("keycode %d is not watched, so %s reports nothing", code, want)
			continue
		}
		if b.objectID != want {
			t.Errorf("keycode %d reports as %q, want %q", code, b.objectID, want)
		}
	}
	if len(buttons) != 2 {
		t.Errorf("the daemon watches %d keys, want 2", len(buttons))
	}
	for code, b := range buttons {
		if !s.HasButton(b.objectID) {
			t.Errorf("keycode %d reports as %q, which internal/esphome has no entity for",
				code, b.objectID)
		}
	}
	for _, objectID := range s.Buttons() {
		watched := false
		for _, b := range buttons {
			if b.objectID == objectID {
				watched = true
			}
		}
		if !watched {
			t.Errorf("internal/esphome lists %q, which no keycode here reports", objectID)
		}
	}
	if got := buttons[113].start; got != button.ModeMonitor {
		t.Errorf("the mute key ships in %v, want %v", got, button.ModeMonitor)
	}
	if got := buttons[138].start; got != button.ModeIntercept {
		t.Errorf("the action key ships in %v, want %v", got, button.ModeIntercept)
	}
}

func TestTheBlueprintFiltersToOneButton(t *testing.T) {
	body, err := os.ReadFile("ha/dot-action-button.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "button: action_button") {
		t.Error("the blueprint does not filter on a button, so every button on the Dot runs it")
	}
	if !strings.Contains(string(body), "trigger.event.data.event_type") {
		t.Error("the blueprint no longer reads event_type")
	}
}

func TestTheCommandNeedsBothAJarAndAnAccount(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		jar, registered, known bool
		want                   bool
	}{
		{"a jar and a registered dot", true, true, true, true},
		{"no jar at all", false, true, true, false},
		{"a jar on a dot with no account", true, false, true, false},
		{"a jar, and no reading either way", true, false, false, true},
		{"no jar, and no reading either way", false, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandReady(tt.jar, tt.registered, tt.known); got != tt.want {
				t.Errorf("commandReady(%v, %v, %v) = %v, want %v",
					tt.jar, tt.registered, tt.known, got, tt.want)
			}
		})
	}
}

func TestTheModelWeSendCarriesNoDotOfItsOwn(t *testing.T) {
	if strings.Contains(deviceModel, ".") {
		t.Errorf("deviceModel is %q; Home Assistant splits project_name on the dot and "+
			"takes [1] as the model, so a dot here truncates it on the device page", deviceModel)
	}
}

func TestUninstallClearsThePersistedSendspinFlag(t *testing.T) {
	script, err := os.ReadFile("deploy/uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{sendspinFlag, sendspinDelay} {
		name := "persist.overdub." + flag
		if !strings.Contains(string(script), name) {
			t.Errorf("deploy/uninstall.sh never names %s, so an uninstall leaves it behind"+
				" and a reinstall starts with a setting nobody chose", name)
		}
	}
}

func TestTheSendspinSwitchTakesTheWholeSurfaceAway(t *testing.T) {
	esphomeAdvert := mdns.Advert{
		Service: "_esphomelib._tcp.local.", Port: apiPort, Records: []string{"mac=x"},
	}
	spy := &advertSpy{}
	client := newRecordingClient()
	ln := newNopListener()
	hold := make(chan struct{})
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: spy,
		base:      []mdns.Advert{esphomeAdvert},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
	}

	toggle.mu.Lock()
	toggle.on = true
	toggle.client = client
	toggle.ln = ln
	toggle.hold = hold
	toggle.mu.Unlock()

	toggle.disable()

	if toggle.On() {
		t.Error("the switch still reports on after disable")
	}
	if len(spy.lists) != 1 {
		t.Fatalf("disable called Advertise %d times, want 1", len(spy.lists))
	}
	if len(spy.lists[0]) != 1 || spy.lists[0][0].Service != esphomeAdvert.Service {
		t.Errorf("disable advertised %v, want only %s", spy.lists[0], esphomeAdvert.Service)
	}
	select {
	case <-hold:
	default:
		t.Error("disable left the re-assert running, so tcp/8928 comes back after the delete")
	}
	select {
	case <-ln.closed:
	default:
		t.Error("disable left the listener open, so the port still accepts")
	}
	select {
	case <-client.closes:
	default:
		t.Error("disable left the sessions up, so a server already admitted keeps playing")
	}
}

func TestSwitchingSendspinNeverBlocksTheCaller(t *testing.T) {
	toggle := &sendspinSwitch{want: make(chan bool, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			toggle.Set(i%2 == 0)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Set blocked with nothing draining the channel")
	}
	if got := len(toggle.want); got != 1 {
		t.Fatalf("the channel holds %d wants, want 1", got)
	}
	if last := <-toggle.want; last != false {
		t.Errorf("the channel held %v, want the newest request (false)", last)
	}
}

type advertSpy struct {
	lists [][]mdns.Advert
}

func (s *advertSpy) Advertise(list []mdns.Advert) error {
	s.lists = append(s.lists, append([]mdns.Advert(nil), list...))
	return nil
}

type nopListener struct{ closed chan struct{} }

func newNopListener() nopListener { return nopListener{closed: make(chan struct{}, 1)} }

func (nopListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l nopListener) Close() error {
	select {
	case l.closed <- struct{}{}:
	default:
	}
	return nil
}
func (nopListener) Addr() net.Addr { return nil }

type recordingClient struct{ closes chan struct{} }

func newRecordingClient() *recordingClient {
	return &recordingClient{closes: make(chan struct{}, 4)}
}

func (*recordingClient) Serve(net.Listener) error { return net.ErrClosed }
func (c *recordingClient) Close() {
	select {
	case c.closes <- struct{}{}:
	default:
	}
}

func TestTogglingSendspinDoesNotHandOutAFreshLogBudget(t *testing.T) {
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		peer: &untrustedlog.Log{Subject: "sendspin"},
	}
	first := sendspinClient(toggle.name, toggle.mac, sendspin.Keys{}, nil, toggle.peer)
	for i := 0; i < untrustedlog.Burst; i++ {
		first.Peer.Printf("sendspin: line %d", i)
	}
	spent := first.Peer.Written()
	if spent != untrustedlog.Burst {
		t.Fatalf("wrote %d lines, want the burst of %d", spent, untrustedlog.Burst)
	}

	next := sendspinClient(toggle.name, toggle.mac, sendspin.Keys{}, nil, toggle.peer)
	next.Peer.Printf("sendspin: after the switch came back")
	if got := next.Peer.Written(); got != spent {
		t.Errorf("the switch-on wrote %d lines against a budget that had already spent %d; "+
			"a toggle must not refill it", got, spent)
	}
}

func TestDisableWaitsForTheReassertBeforeDeletingTheRule(t *testing.T) {
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: &advertSpy{},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
	}
	hold := make(chan struct{})
	reasserting := make(chan struct{})
	toggle.held.Add(1)
	go func() {
		defer toggle.held.Done()
		<-hold
		time.Sleep(150 * time.Millisecond)
		close(reasserting)
	}()

	toggle.mu.Lock()
	toggle.on = true
	toggle.client = newRecordingClient()
	toggle.ln = newNopListener()
	toggle.hold = hold
	toggle.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); toggle.disable() }()

	select {
	case <-done:
		select {
		case <-reasserting:
			t.Fatal("the test's own goroutine finished first; the timing proves nothing")
		default:
			t.Error("disable returned while the re-assert was still running, so DenyTCP raced it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disable never returned")
	case <-reasserting:
	}
	<-done
}

type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) note(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
}

func (l *stepLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

type orderedAdvertiser struct{ log *stepLog }

func (a orderedAdvertiser) Advertise([]mdns.Advert) error {
	a.log.note("advert")
	return nil
}

type orderedListener struct{ log *stepLog }

func (orderedListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l orderedListener) Close() error {
	l.log.note("listener")
	return nil
}
func (orderedListener) Addr() net.Addr { return nil }

type orderedClient struct{ log *stepLog }

func (orderedClient) Serve(net.Listener) error { return net.ErrClosed }
func (c orderedClient) Close()                 { c.log.note("sessions") }

func TestDisableDeletesTheRuleLastOfAll(t *testing.T) {
	steps := &stepLog{}
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		deny: func(int) error {
			steps.note("deny")
			return nil
		},
	}
	hold := make(chan struct{})
	toggle.held.Add(1)
	go func() {
		defer toggle.held.Done()
		<-hold
		time.Sleep(50 * time.Millisecond)
		steps.note("reassert")
	}()

	toggle.mu.Lock()
	toggle.on = true
	toggle.client = orderedClient{steps}
	toggle.ln = orderedListener{steps}
	toggle.hold = hold
	toggle.mu.Unlock()

	if !toggle.disable() {
		t.Fatal("disable refused a switch that was on")
	}

	want := []string{"advert", "reassert", "listener", "sessions", "deny"}
	got := steps.seen()
	if len(got) != len(want) {
		t.Fatalf("teardown did %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("teardown did %v, want %v", got, want)
		}
	}
}

func TestSendspinIsAdvertisedOnlyOnceItsPortIsReachable(t *testing.T) {
	steps := &stepLog{}
	address := make(chan struct{})
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		allow: func(int) error {
			steps.note("allow")
			return nil
		},
		ready: func(<-chan struct{}) bool {
			<-address
			steps.note("address")
			return true
		},
	}

	toggle.mu.Lock()
	toggle.on = true
	toggle.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		toggle.advertiseOnceReachable(make(chan struct{}))
	}()

	time.Sleep(100 * time.Millisecond)
	if got := steps.seen(); len(got) != 0 {
		t.Fatalf("did %v before the port had an address; the advert has to wait", got)
	}
	close(address)
	<-done

	want := []string{"address", "allow", "advert"}
	got := steps.seen()
	if len(got) != len(want) {
		t.Fatalf("startup did %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("startup did %v, want %v: the rule is asserted last before the advert", got, want)
		}
	}
}

func TestSwitchingOffASurfaceThatIsAlreadyOffIsRefusedRatherThanRun(t *testing.T) {
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: &advertSpy{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
	}
	if toggle.disable() {
		t.Error("disable reported it tore down a surface that was already off")
	}
}

func TestATeardownWhileWaitingForAnAddressAdvertisesNothing(t *testing.T) {
	steps := &stepLog{}
	hold := make(chan struct{})
	address := make(chan struct{})
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		allow: func(int) error {
			steps.note("allow")
			return nil
		},
		ready: func(<-chan struct{}) bool {
			<-address
			return true
		},
	}
	toggle.mu.Lock()
	toggle.on = true
	toggle.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		toggle.advertiseOnceReachable(hold)
	}()

	close(hold)
	close(address)
	<-done

	if got := steps.seen(); len(got) != 0 {
		t.Errorf("did %v after the surface was switched off, want nothing: the advert"+
			" would outlive the listener and the rule", got)
	}
}

func TestAnAdvertIsNeverPublishedForASurfaceAlreadySwitchedOff(t *testing.T) {
	steps := &stepLog{}
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		allow:     func(int) error { return nil },
		ready:     func(<-chan struct{}) bool { return true },
	}

	toggle.advertiseOnceReachable(make(chan struct{}))

	for _, step := range steps.seen() {
		if step == "advert" {
			t.Fatalf("published %v for a surface whose `on` was already false", steps.seen())
		}
	}
}

func TestTheClientCarriesTheLeadThisDotNeeds(t *testing.T) {
	client := sendspinClient("kitchen", "00:00:00:00:00:01", sendspin.Keys{}, nil, nil)
	if client.RequiredLeadMS != sendspinLead {
		t.Fatalf("the client declares a %d ms lead where this dot needs %d, so a server may"+
			" send a first chunk sooner than the mapping can be placed",
			client.RequiredLeadMS, sendspinLead)
	}
	page, err := os.ReadFile("docs/sendspin.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), fmt.Sprintf("`required_lead_time_ms` is **%d**",
		sendspinLead)) {
		t.Errorf("docs/sendspin.md does not say the lead is %d, so the page and the wire"+
			" disagree about the one number measured on hardware", sendspinLead)
	}
}

func TestEverySettingThisDotKeepsFitsAPropertyName(t *testing.T) {
	for _, name := range []string{sendspinFlag, sendspinDelay} {
		if err := device.SetNumber(name, 0); err != nil &&
			strings.Contains(err.Error(), "characters") {
			t.Errorf("%s cannot be kept on this device: %v", name, err)
		}
	}
}
