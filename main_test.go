package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
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

func TestUninstallClearsEveryPropertyThisTreeKeeps(t *testing.T) {
	script, err := os.ReadFile("deploy/uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	const glob = "/data/property/persist.overdub.*"
	if !strings.Contains(string(script), glob) {
		t.Fatalf("deploy/uninstall.sh does not enumerate %s, so it clears the names"+
			" somebody remembered rather than the ones this dot has: three properties"+
			" from measuring the name limit sat in flash for weeks that way", glob)
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(glob, "/data/property/"), "*")
	if stem != device.Prefix {
		t.Errorf("deploy/uninstall.sh clears %s and this daemon writes %s*, so every"+
			" property it keeps would survive an uninstall and a reinstall would start"+
			" with settings nobody chose", glob, device.Prefix)
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
		deny:      func(int) error { return nil },
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
	toggle := &sendspinSwitch{
		want: make(chan bool, 1),
		flag: func(bool) error { return nil },
		deny: func(int) error { return nil },
	}
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

type recordingClient struct {
	held int

	closes chan struct{}
	delays chan int
}

func newRecordingClient() *recordingClient {
	return &recordingClient{closes: make(chan struct{}, 4), delays: make(chan int, 4)}
}

func (*recordingClient) Serve(net.Listener) error { return net.ErrClosed }

func (c *recordingClient) Delay() int { return c.held }

func (c *recordingClient) SetDelay(ms int) {
	c.held = ms
	select {
	case c.delays <- ms:
	default:
	}
}
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
	first := sendspinClient(toggle.name, toggle.mac, sendspin.Keys{}, nil, toggle.peer, 0, true, nil)
	for i := 0; i < untrustedlog.Burst; i++ {
		first.Peer.Printf("sendspin: line %d", i)
	}
	spent := first.Peer.Written()
	if spent != untrustedlog.Burst {
		t.Fatalf("wrote %d lines, want the burst of %d", spent, untrustedlog.Burst)
	}

	next := sendspinClient(toggle.name, toggle.mac, sendspin.Keys{}, nil, toggle.peer, 0, true, nil)
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
		deny:      func(int) error { return nil },
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
func (orderedClient) Delay() int               { return 0 }
func (orderedClient) SetDelay(int)             {}
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
	client := sendspinClient("kitchen", "00:00:00:00:00:01", sendspin.Keys{}, nil, nil, 0, true, nil)
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

func TestSettingTheDelayNeverBlocksTheCaller(t *testing.T) {
	toggle := &sendspinSwitch{wantDelay: make(chan int, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ms := 0; ms < 50; ms++ {
			toggle.SetDelay(ms)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetDelay blocked with nothing draining the channel, and the goroutine" +
			" that calls it is the one reading every other frame on that connection")
	}
	if got := len(toggle.wantDelay); got != 1 {
		t.Fatalf("the channel holds %d figures, want 1", got)
	}
	if last := <-toggle.wantDelay; last != 49 {
		t.Errorf("the channel held %d, want the newest figure (49)", last)
	}
}

func TestADelaySetWhileSendspinIsOnGoesToTheLiveClient(t *testing.T) {
	client := newRecordingClient()
	toggle := &sendspinSwitch{
		peer: &untrustedlog.Log{Subject: "sendspin"},
		save: func(int) error {
			t.Error("the switch persisted the delay itself while a client was live, so" +
				" the figure is written by two owners and the one that reports it back" +
				" to the server may not be the one that landed")
			return nil
		},
	}
	toggle.mu.Lock()
	toggle.on, toggle.client = true, client
	toggle.mu.Unlock()

	toggle.applyDelay(1500)

	select {
	case got := <-client.delays:
		if got != 1500 {
			t.Errorf("the live client was told %d ms, want 1500", got)
		}
	default:
		t.Error("the live client was never told, so a delay set in Home Assistant is not" +
			" applied to what is playing and no server is told it moved")
	}
}

func TestADelaySetWhileSendspinIsOffIsStillKept(t *testing.T) {
	var saved []int
	woke := 0
	toggle := &sendspinSwitch{
		peer: &untrustedlog.Log{Subject: "sendspin"},
		wake: func() { woke++ },
		save: func(ms int) error {
			saved = append(saved, ms)
			return nil
		},
	}

	toggle.applyDelay(sendspin.MaxStaticDelayMS + 1000)

	if len(saved) != 1 || saved[0] != sendspin.MaxStaticDelayMS {
		t.Errorf("a delay set with the surface off was kept as %v, want one held at the"+
			" %d ms the spec allows: the next client reads this property for its"+
			" starting figure, so a value out of range would be applied by nothing",
			saved, sendspin.MaxStaticDelayMS)
	}
	if got := toggle.Delay(); got != sendspin.MaxStaticDelayMS {
		t.Errorf("the switch reports %d ms, want %d", got, sendspin.MaxStaticDelayMS)
	}
	if woke != 1 {
		t.Errorf("the api was woken %d times, want 1: the delay entity is read on the"+
			" live poll's heavy tick, so without a wake the control sits at its old"+
			" figure for seconds after somebody moved it", woke)
	}

	toggle.applyDelay(sendspin.MaxStaticDelayMS)
	if len(saved) != 1 {
		t.Errorf("the same figure was written again (%v), and each write is two forks", saved)
	}
}

func TestADelayAServerSetsIsWhatHomeAssistantIsShown(t *testing.T) {
	var saved []int
	woke := 0
	toggle := &sendspinSwitch{
		peer: &untrustedlog.Log{Subject: "sendspin"},
		wake: func() { woke++ },
		save: func(ms int) error {
			saved = append(saved, ms)
			return nil
		},
	}

	toggle.mu.Lock()
	toggle.client = orderedClient{&stepLog{}}
	toggle.mu.Unlock()

	if err := toggle.keepDelay(1814); err != nil {
		t.Fatal(err)
	}

	toggle.mu.Lock()
	held := toggle.delayMS
	toggle.mu.Unlock()
	if held != 1814 {
		t.Errorf("a delay a server set left this switch holding %d ms, want 1814: the"+
			" client keeps the figure and hands it to this same func, and what the"+
			" switch holds is what it reports once that client is gone", held)
	}
	if len(saved) != 1 || saved[0] != 1814 || woke != 1 {
		t.Errorf("a delay a server set was persisted as %v and woke the api %d times,"+
			" want one 1814 and one wake", saved, woke)
	}
}

func TestTheDelayTheClientStartsWithIsTheOneTheSwitchHolds(t *testing.T) {
	toggle := &sendspinSwitch{peer: &untrustedlog.Log{Subject: "sendspin"}, delayMS: 700}
	client := sendspinClient(toggle.name, toggle.mac, sendspin.Keys{}, nil, toggle.peer,
		toggle.Delay(), false, toggle.keepDelay)
	if client.DelayMS != 700 {
		t.Errorf("a client built while the switch held 700 ms starts at %d: the property"+
			" is read once at startup, so a client that reads nowhere starts every"+
			" server at a delay nobody chose", client.DelayMS)
	}
}

func TestAFigureThePropertyRefusedIsNotRecordedAsKept(t *testing.T) {
	toggle := &sendspinSwitch{
		peer:    &untrustedlog.Log{Subject: "sendspin"},
		delayMS: 700,
		save:    func(int) error { return errors.New("did not read back") },
	}
	woke := 0
	toggle.wake = func() { woke++ }

	if err := toggle.keepDelay(1200); err == nil {
		t.Fatal("a property that refused the write reported success")
	}
	if got := toggle.Delay(); got != 700 {
		t.Errorf("a figure the property refused was recorded as %d ms anyway: what this"+
			" switch holds is what the next client is built from, so a figure that never"+
			" reached flash makes it report one that will not survive a reboot", got)
	}
	if woke != 0 {
		t.Errorf("a refused write woke the api %d times, and the control has nothing new"+
			" to show until one lands", woke)
	}
}

type blockingDelayClient struct {
	log  *stepLog
	mu   sync.Mutex
	held int
}

func (*blockingDelayClient) Serve(net.Listener) error { return net.ErrClosed }
func (c *blockingDelayClient) Delay() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

func (c *blockingDelayClient) SetDelay(ms int) {
	c.log.note("delay in")
	time.Sleep(60 * time.Millisecond)
	c.mu.Lock()
	c.held = ms
	c.mu.Unlock()
	c.log.note("delay out")
}
func (c *blockingDelayClient) Close() { c.log.note("sessions") }

func waitForStep(t *testing.T, steps *stepLog, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !slices.Contains(steps.seen(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %q; the switch logged %v", want, steps.seen())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type slowAdvertiser struct{ log *stepLog }

func (a slowAdvertiser) Advertise([]mdns.Advert) error {
	a.log.note("advert in")
	time.Sleep(80 * time.Millisecond)
	a.log.note("advert out")
	return nil
}

func TestASetPutsNothingInTheMiddleOfAToggleInFlight(t *testing.T) {
	steps := &stepLog{}
	client := &blockingDelayClient{log: steps}
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: slowAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		save:      func(int) error { steps.note("kept"); return nil },
		deny:      func(int) error { steps.note("deny"); return nil },
		flag:      func(bool) error { return nil },
	}
	hold := make(chan struct{})
	toggle.mu.Lock()
	toggle.on, toggle.client, toggle.ln, toggle.hold = true, client, orderedListener{steps}, hold
	toggle.mu.Unlock()
	go toggle.run()

	toggle.Set(false)
	waitForStep(t, steps, "advert in")
	toggle.SetDelay(900)
	waitForStep(t, steps, "advert out")
	waitForStep(t, steps, "kept")

	got := steps.seen()
	for i, step := range got {
		if step != "advert in" {
			continue
		}
		if i+1 >= len(got) || got[i+1] != "advert out" {
			t.Fatalf("a set landed inside a toggle that was still being applied, as %v:"+
				" whether there is a client to hand a figure to is only sound if nothing"+
				" can retire or publish one while that figure is being handed over", got)
		}
	}
}

func TestTheLastOfSeveralRapidSetsIsTheOneThisClientHolds(t *testing.T) {
	steps := &stepLog{}
	client := &blockingDelayClient{log: steps}
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		save:      func(int) error { return nil },
	}
	toggle.mu.Lock()
	toggle.on, toggle.client = true, client
	toggle.mu.Unlock()
	go toggle.run()

	toggle.SetDelay(700)
	waitForStep(t, steps, "delay in")
	toggle.SetDelay(750)
	toggle.SetDelay(800)

	deadline := time.Now().Add(5 * time.Second)
	for client.Delay() != 800 {
		if time.Now().After(deadline) {
			t.Fatalf("the last of three rapid sets left this client holding %d ms, so a"+
				" figure the operator has already replaced is the one it plays at: a set"+
				" that arrives while the worker is busy displaces the one waiting rather"+
				" than being dropped in its favour", client.Delay())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSwitchingOffWaitsForTheKeeperToWriteWhatItHolds(t *testing.T) {
	steps := &stepLog{}
	served := make(chan struct{})
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		deny:      func(int) error { return nil },
	}
	hold := make(chan struct{})
	toggle.mu.Lock()
	toggle.on, toggle.client, toggle.ln = true, orderedClient{steps}, orderedListener{steps}
	toggle.served, toggle.hold = served, hold
	toggle.mu.Unlock()
	go func() {
		time.Sleep(80 * time.Millisecond)
		close(served)
	}()

	toggle.disable()

	select {
	case <-served:
	default:
		t.Error("switching off returned while its client was still serving, so the next" +
			" switch-on reads the property while the keeper is still writing it and can" +
			" build a client at the figure somebody has just replaced")
	}
}

type closingListener struct {
	mu     sync.Mutex
	closed bool
}

func (*closingListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l *closingListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}
func (*closingListener) Addr() net.Addr { return nil }
func (l *closingListener) shut() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func TestStoppingWaitsForTheDelayKeeperToWriteWhatItHolds(t *testing.T) {
	ln := &closingListener{}
	served := make(chan struct{})
	toggle := &sendspinSwitch{peer: &untrustedlog.Log{Subject: "sendspin"}}
	toggle.mu.Lock()
	toggle.on, toggle.ln, toggle.served = true, ln, served
	toggle.mu.Unlock()

	wrote := make(chan struct{})
	go func() {
		<-time.After(80 * time.Millisecond)
		close(wrote)
		close(served)
	}()
	toggle.flush()

	if !ln.shut() {
		t.Error("stopping left the listener open, so this client goes on serving and its" +
			" keeper is never told to write what it is holding")
	}
	select {
	case <-wrote:
	default:
		t.Error("stopping returned before the keeper had written: os.Exit runs no" +
			" deferred function, so a figure still inside the window is gone -- and the" +
			" figure an operator just typed is the one worth waiting 40 ms for")
	}
}

func TestStoppingDoesNotWaitForeverOnAKeeperThatWillNotFinish(t *testing.T) {
	ln := &closingListener{}
	toggle := &sendspinSwitch{
		peer:     &untrustedlog.Log{Subject: "sendspin"},
		flushFor: 60 * time.Millisecond,
	}
	toggle.mu.Lock()
	toggle.on, toggle.ln, toggle.served = true, ln, make(chan struct{})
	toggle.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		toggle.flush()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping waited on a client that never finished serving, so a wedged" +
			" write holds the daemon open and the supervisor cannot see why")
	}
}

func TestStoppingTheDaemonFlushesTheDelayItHolds(t *testing.T) {
	ln := &closingListener{}
	served := make(chan struct{})
	close(served)
	toggle := &sendspinSwitch{peer: &untrustedlog.Log{Subject: "sendspin"}}
	toggle.mu.Lock()
	toggle.on, toggle.ln, toggle.served = true, ln, served
	toggle.mu.Unlock()
	switched.Store(toggle)
	t.Cleanup(func() { switched.Store(nil) })

	stopping()

	if !ln.shut() {
		t.Error("a daemon on its way out left sendspin serving, so the figure an operator" +
			" set inside the keeper's window goes with the process: os.Exit runs no" +
			" deferred function, and SIGTERM is what an install stops the old one with")
	}
}

func offSwitch(t *testing.T, saved *[]int, mu *sync.Mutex) *sendspinSwitch {
	t.Helper()
	return &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{&stepLog{}},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		keepEvery: 30 * time.Second,
		save: func(ms int) error {
			mu.Lock()
			defer mu.Unlock()
			*saved = append(*saved, ms)
			return nil
		},
		kept: func() (int, bool) {
			mu.Lock()
			defer mu.Unlock()
			if len(*saved) == 0 {
				return 0, true
			}
			return (*saved)[len(*saved)-1], true
		},
		flag:     func(bool) error { return nil },
		allow:    func(int) error { return nil },
		deny:     func(int) error { return nil },
		holdOpen: func(<-chan struct{}) {},
	}
}

func TestSetsWithSendspinOffCostOneWritePerWindow(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	toggle := offSwitch(t, &saved, &mu)
	go toggle.run()

	toggle.SetDelay(100)
	waitUntil(t, "the first figure to reach the property", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) == 1 && saved[0] == 100
	})

	for _, ms := range []int{200, 300, 400, 500, 600} {
		toggle.SetDelay(ms)
	}
	waitUntil(t, "the last figure to be the one this switch holds", func() bool {
		return toggle.Delay() == 600
	})

	mu.Lock()
	wrote := len(saved)
	mu.Unlock()
	if wrote != 1 {
		t.Errorf("five more sets inside one window wrote %d times, want the one write"+
			" already made: with no client there is no keeper to hand a figure to, and"+
			" writing each one puts an automation that alternates values straight onto"+
			" flash at a setprop apiece", wrote)
	}

	toggle.flush()

	mu.Lock()
	defer mu.Unlock()
	if len(saved) == 0 || saved[len(saved)-1] != 600 {
		t.Errorf("stopping wrote %v, want the held 600 last: a figure inside the window"+
			" is held rather than dropped, so the stop that ends the window has to write"+
			" it or the operator's last change is the one that never survives", saved)
	}
}

func TestAFigureHeldInsideTheWindowStillReachesTheControl(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	woke := 0
	toggle := offSwitch(t, &saved, &mu)
	toggle.wake = func() {
		mu.Lock()
		defer mu.Unlock()
		woke++
	}
	go toggle.run()

	toggle.SetDelay(100)
	waitUntil(t, "the first figure to reach the property", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) == 1
	})

	toggle.SetDelay(200)
	waitUntil(t, "the held figure to be the one this switch reports", func() bool {
		return toggle.Delay() == 200
	})
	waitUntil(t, "the api to be woken for a figure that is held rather than written",
		func() bool {
			mu.Lock()
			defer mu.Unlock()
			return woke == 2
		})

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 {
		t.Errorf("the second figure was written as %v rather than held: this test says"+
			" nothing unless the figure it asks about is one the window is holding back",
			saved)
	}
}

func TestSwitchingOnWritesWhatTheWindowWasHolding(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	toggle := offSwitch(t, &saved, &mu)

	toggle.SetDelay(100)
	toggle.applyDelay(100)
	mu.Lock()
	saved = nil
	mu.Unlock()

	toggle.applyDelay(450)
	mu.Lock()
	held := len(saved)
	mu.Unlock()
	if held != 0 {
		t.Fatalf("the second figure was written straight away as %v, so this test never"+
			" reaches the case it is about", saved)
	}

	t.Cleanup(func() { toggle.disable() })
	toggle.applyWant(true)

	mu.Lock()
	defer mu.Unlock()
	if len(saved) == 0 || saved[0] != 450 {
		t.Errorf("switching on wrote %v, want the held 450 first: a switch-on builds its"+
			" client from what the property holds, so a figure still inside the window"+
			" when it happens is one the client starts at the wrong figure without",
			saved)
	}
}

func TestAFigureThisSwitchCannotWriteIsGivenUpOn(t *testing.T) {
	var mu sync.Mutex
	tried := 0
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{&stepLog{}},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		keepEvery: 20 * time.Millisecond,
		save: func(int) error {
			mu.Lock()
			defer mu.Unlock()
			tried++
			return errors.New("setprop refused")
		},
	}
	go toggle.run()

	toggle.SetDelay(300)
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return tried
	}
	waitUntil(t, "the writes to be given up on", func() bool {
		return count() >= sendspin.KeepTries
	})
	time.Sleep(200 * time.Millisecond)

	if got := count(); got != sendspin.KeepTries {
		t.Errorf("a figure the property would not take was written %d times, want %d:"+
			" retrying forever costs a setprop every window for the rest of the boot,"+
			" which is the unbounded write this window exists to prevent", got,
			sendspin.KeepTries)
	}
	if got := toggle.Delay(); got != 300 {
		t.Errorf("a figure given up on reads back as %d, want the 300 that was set: the"+
			" write is what failed, and the figure still applies until the next restart",
			got)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAWriteInFlightDoesNotOverwriteAFigureSetWhileItRan(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	started := make(chan struct{})
	release := make(chan struct{})
	toggle := offSwitch(t, &saved, &mu)
	toggle.keepEvery = time.Hour
	toggle.save = func(ms int) error {
		mu.Lock()
		saved = append(saved, ms)
		first := len(saved) == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
		return nil
	}

	go toggle.applyDelay(100)
	<-started
	go toggle.applyDelay(600)
	waitUntil(t, "the second figure to be the one this switch holds", func() bool {
		return toggle.Delay() == 600
	})
	close(release)

	waitUntil(t, "the write that was in flight to be recorded", func() bool {
		toggle.mu.Lock()
		defer toggle.mu.Unlock()
		return toggle.diskMS == 100
	})
	if got := toggle.Delay(); got != 600 {
		t.Errorf("a figure set while a write was in flight reads back as %d, want 600: a"+
			" write records what the property now holds, and recording it as what the"+
			" switch holds puts a figure the operator has already replaced back on the"+
			" control and into the next client", got)
	}
}

func TestSwitchingOnKeepsTheFigureWhenThePropertyCannotBeRead(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	toggle := offSwitch(t, &saved, &mu)
	toggle.kept = func() (int, bool) { return 0, false }
	toggle.mu.Lock()
	toggle.delayMS, toggle.diskMS = 600, 600
	toggle.mu.Unlock()

	t.Cleanup(func() { toggle.disable() })
	toggle.applyWant(true)

	toggle.mu.Lock()
	held := toggle.delayMS
	toggle.mu.Unlock()
	if held != 600 {
		t.Errorf("a switch-on whose property read failed took %d ms, want the 600 this"+
			" switch already held: a read that failed is not a figure, and building the"+
			" client from one hands a correcting keeper a zero to write over whatever"+
			" flash actually has", held)
	}
}

func TestAnUnreadablePropertyIsWrittenWithoutWaitingForAChange(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	toggle := offSwitch(t, &saved, &mu)
	toggle.tookDelay(450, false)
	go toggle.run()

	waitUntil(t, "the figure to be written although nothing changed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) == 1 && saved[0] == 450
	})
}

func TestASecondFigureGetsItsOwnAttemptsAfterOneIsGivenUpOn(t *testing.T) {
	var mu sync.Mutex
	tried := map[int]int{}
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{&stepLog{}},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		keepEvery: 20 * time.Millisecond,
		save: func(ms int) error {
			mu.Lock()
			defer mu.Unlock()
			tried[ms]++
			return errors.New("setprop refused")
		},
	}
	go toggle.run()

	count := func(ms int) int {
		mu.Lock()
		defer mu.Unlock()
		return tried[ms]
	}
	toggle.SetDelay(300)
	waitUntil(t, "the first figure to be given up on", func() bool {
		return count(300) >= sendspin.KeepTries
	})
	toggle.SetDelay(400)
	waitUntil(t, "the second figure to be given up on", func() bool {
		return count(400) >= sendspin.KeepTries
	})
	time.Sleep(200 * time.Millisecond)

	if got := count(400); got != sendspin.KeepTries {
		t.Errorf("a second figure was written %d times, want %d: the attempts are"+
			" counted per figure, so a figure given up on must not spend the attempts"+
			" of the one somebody sets after it", got, sendspin.KeepTries)
	}
}

func TestTwoStopsAtOnceCostOneWrite(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	toggle := offSwitch(t, &saved, &mu)
	toggle.keepEvery = time.Hour
	toggle.save = func(ms int) error {
		mu.Lock()
		saved = append(saved, ms)
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	toggle.mu.Lock()
	toggle.delayMS = 300
	toggle.mu.Unlock()

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toggle.flush()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0] != 300 {
		t.Errorf("two stops arriving together wrote %v, want one 300: deciding whether a"+
			" write is owed and recording that it happened are one operation, and two"+
			" callers that each decide before either records both spend a flash write"+
			" and both count their attempt against a bound neither can see", saved)
	}
}

func TestAnUnreadablePropertyIsStillGivenUpOn(t *testing.T) {
	var mu sync.Mutex
	tried := 0
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{&stepLog{}},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		keepEvery: 10 * time.Millisecond,
		save: func(int) error {
			mu.Lock()
			defer mu.Unlock()
			tried++
			return errors.New("setprop refused")
		},
	}
	toggle.tookDelay(450, false)
	go toggle.run()

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return tried
	}
	waitUntil(t, "the writes to be given up on", func() bool {
		return count() >= sendspin.KeepTries
	})
	time.Sleep(300 * time.Millisecond)

	if got := count(); got != sendspin.KeepTries {
		t.Errorf("a property that could not be read was written %d times, want %d: a"+
			" disk nobody could read is a reason to write once, not a reason exempt"+
			" from the bound, and retrying it forever is a setprop and a log line every"+
			" window for the rest of the boot", got, sendspin.KeepTries)
	}
}

type heldDelayClient struct {
	log *stepLog
	ms  int
}

func (heldDelayClient) Serve(net.Listener) error { return net.ErrClosed }
func (c heldDelayClient) Delay() int             { return c.ms }
func (heldDelayClient) SetDelay(int)             {}
func (c heldDelayClient) Close()                 { c.log.note("sessions") }

func TestSwitchingOffKeepsTheFigureItsClientWasHolding(t *testing.T) {
	steps := &stepLog{}
	served := make(chan struct{})
	close(served)
	toggle := &sendspinSwitch{
		name: "kitchen", mac: "00:00:00:00:00:01",
		responder: orderedAdvertiser{steps},
		base:      []mdns.Advert{},
		want:      make(chan bool, 1),
		wantDelay: make(chan int, 1),
		peer:      &untrustedlog.Log{Subject: "sendspin"},
		deny:      func(int) error { return nil },
	}
	hold := make(chan struct{})
	toggle.mu.Lock()
	toggle.on, toggle.client, toggle.ln = true, heldDelayClient{steps, 1500}, orderedListener{steps}
	toggle.served, toggle.hold, toggle.delayMS, toggle.diskMS = served, hold, 300, 300
	toggle.mu.Unlock()

	toggle.disable()

	if got := toggle.Delay(); got != 1500 {
		t.Errorf("switching off left the control reading %d ms, want the 1500 its client"+
			" was holding: the client's own keeper writes that figure as it stops, and a"+
			" switch that does not take it reports one the property no longer holds and"+
			" will not write over it, because it believes nothing is owed", got)
	}
}

func TestAPropertyThatReadsCleanlyArmsNoWriteAtAll(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	toggle := offSwitch(t, &saved, &mu)
	toggle.tookDelay(450, true)
	go toggle.run()

	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 0 {
		t.Errorf("a property that was read cleanly was written %v anyway: the figure on"+
			" disk is the figure this switch holds, so a write at startup is a flash"+
			" write that says what the property already says", saved)
	}
}

func TestSwitchingOnKeepsAFigureThePropertyRefused(t *testing.T) {
	var mu sync.Mutex
	var saved []int
	refuse := true
	toggle := offSwitch(t, &saved, &mu)
	toggle.kept = func() (int, bool) { return 0, true }
	toggle.save = func(ms int) error {
		mu.Lock()
		defer mu.Unlock()
		if refuse {
			return errors.New("setprop refused")
		}
		saved = append(saved, ms)
		return nil
	}
	toggle.tookDelay(0, true)

	toggle.applyDelay(900)
	if got := toggle.Delay(); got != 900 {
		t.Fatalf("a figure whose write was refused reads back as %d, want 900", got)
	}

	t.Cleanup(func() { toggle.disable() })
	toggle.applyWant(true)

	if got := toggle.Delay(); got != 900 {
		t.Errorf("switching on reverted the delay to %d, want the 900 the operator set:"+
			" the property is read to build the next client, and a figure this switch"+
			" is still holding because the write was refused is not one the property"+
			" can answer for", got)
	}
}
