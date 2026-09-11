package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/button"
	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/esphome"
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
	want := fmt.Sprintf("-i %s -p tcp --dport %d -j ACCEPT", wifiIface, apiPort)
	if !strings.Contains(string(script), want) {
		t.Errorf("deploy/uninstall.sh deletes no rule matching %q, and the daemon adds exactly that",
			want)
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
	s := esphome.NewServer("kitchen", "Echo Dot", "00:00:5E:00:53:2A", nil)

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
