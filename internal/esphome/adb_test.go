package esphome

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForLog(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("the log to say %q", want), func() bool {
		return strings.Contains(out.String(), want)
	})
}

func TestTheSensorPollPutsTheRuleBack(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	held := make(chan struct{}, 4)
	s.adbHold = func() error {
		select {
		case held <- struct{}{}:
		default:
		}
		return nil
	}

	go s.PollSensors(MinSensorTick)
	for i := range 2 {
		if i > 0 {
			select {
			case s.sensorWake <- struct{}{}:
			default:
			}
		}
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatalf("poll %d never asserted the rule: netd's next rebuild closes tcp/5555"+
				" for good, and network adb is the way into a Dot with no cable", i+1)
		}
	}
}

func TestSetADBKeysIsAnActionWithOneTextArgument(t *testing.T) {
	s := testServer(t, testPSK(t))
	found := 0
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListService) || string(entity[1].data) != "set_adb_keys" {
			continue
		}
		found++
		if uint32(entity[2].num) != s.keyADBKeys {
			t.Errorf("the action's key is %d, want %d", entity[2].num, s.keyADBKeys)
		}
	}
	if found != 1 {
		t.Errorf("set_adb_keys was listed %d times, want once: a text entity stops at 255"+
			" characters, and one adb key is about 700", found)
	}
}

func TestTheActionHandsTheWholeTextToTheDevice(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	got := make(chan string, 4)
	s.adbKeys = func(text string) (int, error) {
		got <- text
		if strings.HasPrefix(text, "bad") {
			return 0, errors.New("line 1 is not an adb public key")
		}
		return 2, nil
	}
	s.adbNames = func() ([]string, []string, bool) {
		return []string{"rooter@mac"}, []string{"one@host", "two@host"}, true
	}
	keys := "QAAAA one@host\nQAAAB two@host"
	if err := s.handle(&conn{sock: fakeAddr{}}, msgExecuteService,
		serviceCall(s.keyADBKeys, keys)); err != nil {
		t.Fatal(err)
	}
	if text := <-got; text != keys {
		t.Errorf("the device was handed %q, want the keys whole", text)
	}
	waitForLog(t, &out, "set 2 adb key(s)")
	if list, _ := s.keyList(); list != "rooter@mac 🔒, one@host, two@host" {
		t.Errorf("the adb_keys sensor reads %q", list)
	}

	if err := s.handle(&conn{sock: fakeAddr{}}, msgExecuteService,
		serviceCall(s.keyADBKeys, "bad")); err != nil {
		t.Fatal(err)
	}
	<-got
	waitForLog(t, &out, "were refused")
	if list, _ := s.keyList(); list != "refused: line 1 is not an adb public key;"+
		" rooter@mac 🔒, one@host, two@host" {
		t.Errorf("after a refusal the sensor reads %q, want the reason: the action reports"+
			" no result to Home Assistant, and the log needs the adb being set up", list)
	}

	if err := s.handle(&conn{sock: fakeAddr{}}, msgExecuteService,
		serviceCall(s.keyCommand, keys)); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-got:
		t.Errorf("another action's call reached the adb keys: %q", text)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTheKeyListSaysNoneAndStaysUnderAState(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.adbNames = func() ([]string, []string, bool) { return []string{"rooter@mac"}, nil, true }
	if list, ok := s.keyList(); !ok || list != "rooter@mac 🔒" {
		t.Errorf("with no keys set the sensor reads %q, %v; want the rooting computer's key,"+
			" marked as the one that unlocked the Dot", list, ok)
	}
	s.adbNames = func() ([]string, []string, bool) { return nil, nil, true }
	if list, ok := s.keyList(); !ok || list != "none" {
		t.Errorf("with no keys at all the sensor reads %q, %v; want none", list, ok)
	}
	var many []string
	for range 16 {
		many = append(many, strings.Repeat("n", 40)+"@host")
	}
	s.adbNames = func() ([]string, []string, bool) { return nil, many, true }
	if list, _ := s.keyList(); len(list) > keyListMax {
		t.Errorf("the sensor reads %d characters; Home Assistant refuses a state past %d",
			len(list), keyListMax)
	}
}

func TestALaterSetOfKeysAlwaysWins(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	got := make(chan string, 4)
	s.adbKeys = func(text string) (int, error) {
		got <- text
		return 1, nil
	}
	s.adbNames = func() ([]string, []string, bool) { return nil, nil, true }
	s.adbSetMu.Lock()
	for _, text := range []string{"QAAAA revoked@host", "none"} {
		if err := s.handle(&conn{sock: fakeAddr{}}, msgExecuteService,
			serviceCall(s.keyADBKeys, text)); err != nil {
			t.Fatal(err)
		}
	}
	s.adbSetMu.Unlock()
	if text := <-got; text != "none" {
		t.Errorf("the device was handed %q first, want the later none: keys the user"+
			" revoked would keep opening a root shell", text)
	}
	waitForLog(t, &out, "a later call replaced")
	select {
	case text := <-got:
		t.Errorf("the overtaken call still reached the device: %q", text)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTheKeyListCutsOnACharacter(t *testing.T) {
	s := testServer(t, testPSK(t))
	name := strings.Repeat("é", 200)
	s.adbNames = func() ([]string, []string, bool) { return []string{name}, nil, true }
	list, _ := s.keyList()
	if !utf8.ValidString(list) || len(list) > keyListMax {
		t.Errorf("the sensor reads %d bytes, valid UTF-8 %v; Home Assistant refuses a"+
			" state that is not UTF-8", len(list), utf8.ValidString(list))
	}
}
