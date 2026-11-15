package device

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

func fakeProc(t *testing.T) {
	t.Helper()
	oldProc, oldWait, oldStill, oldRead, oldSet := procDir, readerWait, readerStill, readProp, setProp
	t.Cleanup(func() {
		procDir, readerWait, readerStill, readProp, setProp = oldProc, oldWait, oldStill, oldRead, oldSet
	})
	procDir = t.TempDir()
	readerStill = 20 * time.Millisecond
	readerWait = 300 * time.Millisecond
}

func fakeProcess(t *testing.T, pid, exe string, fds ...string) {
	t.Helper()
	dir := filepath.Join(procDir, pid)
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(exe+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, target := range fds {
		if err := os.Symlink(target, filepath.Join(dir, "fd", string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
}

const clone = "/dev/input/event3"

func TestAReaderOnTheDeadCloneIsRestartedOntoTheLiveOne(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "485", "/system/bin/acebuttond", clone+" (deleted)")
	fakeProcess(t, "246", "/system/bin/ace_usagestat_protod", clone)
	readProp = func(string) (string, error) { return "running", nil }
	var set []string
	setProp = func(key, value string) error {
		set = append(set, key+"="+value)
		os.RemoveAll(filepath.Join(procDir, "485"))
		fakeProcess(t, "2817", "/system/bin/acebuttond", clone)
		return nil
	}
	restarted, err := RestartInputReaders(clone, noNode)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restarted, []string{"acebuttond"}) ||
		!slices.Equal(set, []string{"ctl.restart=acebuttond"}) {
		t.Errorf("restarted %v by %v, want acebuttond alone: the other already reads the"+
			" live clone, and a restart would drop a press for nothing", restarted, set)
	}
}

func TestOnlyARunningServiceIsRestarted(t *testing.T) {
	fakeProc(t)
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "stopped", nil
		}
		return "", nil
	}
	setProp = func(key, value string) error {
		t.Errorf("setprop %s %s: a stopped service was started, and one this Dot lacks"+
			" was asked for", key, value)
		return nil
	}
	if restarted, err := RestartInputReaders(clone, noNode); err != nil || len(restarted) > 0 {
		t.Errorf("restarted %v, %v; want nothing", restarted, err)
	}
}

func TestARestartIsReadBackNotAssumed(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "485", "/system/bin/acebuttond", clone+" (deleted)")
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "running", nil
		}
		return "", nil
	}
	setProp = func(string, string) error { return nil }
	if _, err := RestartInputReaders(clone, noNode); err == nil {
		t.Error("a restart init ignored was reported as done; setprop exits 0 either way")
	}
}

func TestAFailedReadIsNamedNotTakenForNoService(t *testing.T) {
	fakeProc(t)
	readProp = func(string) (string, error) { return "", errors.New("getprop: killed") }
	setProp = func(string, string) error { return nil }
	if _, err := RestartInputReaders(clone, noNode); err == nil {
		t.Error("a getprop that failed read as a Dot without the service")
	}
}

func TestAFailedRestartIsNamed(t *testing.T) {
	fakeProc(t)
	readProp = func(string) (string, error) { return "running", nil }
	setProp = func(string, string) error { return errors.New("killed") }
	if _, err := RestartInputReaders(clone, noNode); err == nil {
		t.Error("a restart that failed reported none")
	}
}

func TestOneReaderFailingDoesNotStrandTheOther(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "485", "/system/bin/acebuttond", clone+" (deleted)")
	fakeProcess(t, "246", "/system/bin/ace_usagestat_protod", clone+" (deleted)")
	readProp = func(string) (string, error) { return "running", nil }
	setProp = func(_, service string) error {
		if service == "acebuttond" {
			return errors.New("killed")
		}
		os.RemoveAll(filepath.Join(procDir, "246"))
		fakeProcess(t, "300", "/system/bin/ace_usagestat_protod", clone)
		return nil
	}
	restarted, err := RestartInputReaders(clone, noNode)
	if err == nil || !slices.Equal(restarted, []string{"usagestat_protod"}) {
		t.Errorf("restarted %v with %v, want usagestat_protod and acebuttond's error: the"+
			" two are independent, and the second would poll the dead clone till reboot",
			restarted, err)
	}
}

func TestAReaderOnTheCloneAndADestroyedNodeIsRestarted(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "2399", "/system/bin/acebuttond", clone, "/dev/input/event5 (deleted)")
	fakeProcess(t, "246", "/system/bin/ace_usagestat_protod", clone)
	readProp = func(string) (string, error) { return "running", nil }
	var set []string
	setProp = func(key, value string) error {
		set = append(set, key+"="+value)
		os.RemoveAll(filepath.Join(procDir, "2399"))
		fakeProcess(t, "5501", "/system/bin/acebuttond", clone)
		return nil
	}
	restarted, err := RestartInputReaders(clone, noNode)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restarted, []string{"acebuttond"}) ||
		!slices.Equal(set, []string{"ctl.restart=acebuttond"}) {
		t.Errorf("restarted %v by %v, want acebuttond alone: it polls the speaker's AVRCP"+
			" node after the speaker left, and the other holds nothing dead", restarted, set)
	}
}

func TestADeletedFileOutsideTheInputsIsNotADeadNode(t *testing.T) {
	fakeProc(t)
	fakeProcess(t, "2399", "/system/bin/acebuttond", clone, "/data/local/tmp/log (deleted)")
	if live, dead, _ := held("/system/bin/acebuttond", clone); !live || dead {
		t.Errorf("held = %v, %v; want the clone and nothing dead", live, dead)
	}
}

func fakeInputs(t *testing.T, nodes map[string]string) {
	t.Helper()
	was := inputDir
	t.Cleanup(func() { inputDir = was })
	inputDir = filepath.Join(t.TempDir(), "dev", "input")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for node, name := range nodes {
		if err := os.WriteFile(filepath.Join(inputDir, node), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func noNode(string) (string, error) { return "", errors.New("not an input node") }

func fileName(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func inputNodes(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(inputDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		nodes[e.Name()] = string(b)
	}
	return nodes
}

func asideNodes(t *testing.T) []string {
	t.Helper()
	aside, _ := filepath.Glob(filepath.Join(filepath.Dir(inputDir), asidePrefix+"*"))
	return aside
}

func TestASpeakerNodeIsAsideWhileAReaderStarts(t *testing.T) {
	fakeProc(t)
	fakeInputs(t, map[string]string{"event0": "gpio-keys", "event3": "keys", "event5": "AVRCP"})
	live := filepath.Join(inputDir, "event3")
	fakeProcess(t, "485", "/system/bin/acebuttond", filepath.Join(inputDir, "event5 (deleted)"))
	fakeProcess(t, "246", "/system/bin/ace_usagestat_protod", live)
	readProp = func(string) (string, error) { return "running", nil }
	var seen map[string]string
	setProp = func(string, string) error {
		seen = inputNodes(t)
		os.RemoveAll(filepath.Join(procDir, "485"))
		fakeProcess(t, "2817", "/system/bin/acebuttond", live)
		return nil
	}
	if _, err := RestartInputReaders(live, fileName); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"event0": "gpio-keys", "event3": "keys"}; !maps.Equal(seen, want) {
		t.Errorf("acebuttond started with %v, want %v: it would keep the speaker's node,"+
			" and the disconnect would restart it, and the restart plays the privacy earcon",
			seen, want)
	}
	if got, want := inputNodes(t), map[string]string{"event0": "gpio-keys", "event3": "keys",
		"event5": "AVRCP"}; !maps.Equal(got, want) || len(asideNodes(t)) > 0 {
		t.Errorf("after the restart the inputs were %v, with %v aside; want %v and nothing aside",
			got, asideNodes(t), want)
	}
}

func TestASpeakerNodeStaysAsideUntilTheReaderIsStill(t *testing.T) {
	fakeProc(t)
	readerWait = 5 * time.Second
	readerStill = 200 * time.Millisecond
	fakeInputs(t, map[string]string{"event0": "gpio-keys", "event3": "keys", "event5": "AVRCP"})
	live := filepath.Join(inputDir, "event3")
	fakeProcess(t, "485", "/system/bin/acebuttond", filepath.Join(inputDir, "event5 (deleted)"))
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "running", nil
		}
		return "", nil
	}
	late := make(chan map[string]string, 1)
	setProp = func(string, string) error {
		os.RemoveAll(filepath.Join(procDir, "485"))
		fakeProcess(t, "2817", "/system/bin/acebuttond", live)
		go func() {
			time.Sleep(100 * time.Millisecond)
			seen := inputNodes(t)
			if err := os.Symlink(filepath.Join(inputDir, "event0"),
				filepath.Join(procDir, "2817", "fd", "9")); err != nil {
				t.Error(err)
			}
			late <- seen
		}()
		return nil
	}
	if _, err := RestartInputReaders(live, fileName); err != nil {
		t.Fatal(err)
	}
	if seen := <-late; seen["event5"] != "" {
		t.Error("the speaker's node was back while acebuttond was still opening nodes," +
			" so it could open it, and the disconnect would restart it")
	}
	if inputNodes(t)["event5"] != "AVRCP" {
		t.Error("the speaker's node was not put back")
	}
}

func TestAHolderOfTheSpeakerNodeStillSeesItInTheInputs(t *testing.T) {
	fakeProc(t)
	fakeInputs(t, map[string]string{"event3": "keys", "event5": "AVRCP"})
	live := filepath.Join(inputDir, "event3")
	node := filepath.Join(inputDir, "event5")
	f, err := os.Open(node)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fakeProcess(t, "485", "/system/bin/acebuttond", filepath.Join(inputDir, "event5 (deleted)"))
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "running", nil
		}
		return "", nil
	}
	setProp = func(string, string) error {
		os.RemoveAll(filepath.Join(procDir, "485"))
		fakeProcess(t, "2817", "/system/bin/acebuttond", live)
		return nil
	}
	if _, err := RestartInputReaders(live, fileName); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join("/proc/self/fd", strconv.Itoa(int(f.Fd()))))
	if err != nil {
		t.Fatal(err)
	}
	if got != node {
		t.Errorf("a reader holding the speaker's node now holds %s, want %s: after the"+
			" disconnect its dead node would be outside the inputs, and never restarted", got, node)
	}
}

func TestANodeRemadeWhileAsideIsKept(t *testing.T) {
	fakeProc(t)
	fakeInputs(t, map[string]string{"event3": "keys", "event5": "AVRCP"})
	live := filepath.Join(inputDir, "event3")
	fakeProcess(t, "485", "/system/bin/acebuttond", filepath.Join(inputDir, "event5 (deleted)"))
	readProp = func(name string) (string, error) {
		if name == "init.svc.acebuttond" {
			return "running", nil
		}
		return "", nil
	}
	setProp = func(string, string) error {
		if err := os.WriteFile(filepath.Join(inputDir, "event5"), []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.RemoveAll(filepath.Join(procDir, "485"))
		fakeProcess(t, "2817", "/system/bin/acebuttond", live)
		return nil
	}
	if _, err := RestartInputReaders(live, fileName); err != nil {
		t.Fatal(err)
	}
	if got := inputNodes(t)["event5"]; got != "new" || len(asideNodes(t)) > 0 {
		t.Errorf("event5 holds %q with %v aside; want the new node and nothing aside", got, asideNodes(t))
	}
}

func TestANodeLeftAsideIsPutBack(t *testing.T) {
	fakeProc(t)
	fakeInputs(t, map[string]string{"event3": "keys"})
	aside := filepath.Join(filepath.Dir(inputDir), asidePrefix+"event5")
	if err := os.WriteFile(aside, []byte("AVRCP"), 0o600); err != nil {
		t.Fatal(err)
	}
	readProp = func(string) (string, error) { return "stopped", nil }
	if _, err := RestartInputReaders(filepath.Join(inputDir, "event3"), fileName); err != nil {
		t.Fatal(err)
	}
	if got := inputNodes(t)["event5"]; got != "AVRCP" || len(asideNodes(t)) > 0 {
		t.Errorf("event5 holds %q with %v aside; a daemon stopped mid-restart would leave the"+
			" speaker's node hidden", got, asideNodes(t))
	}
}

func TestAnInputNodeGoneIsSeenByName(t *testing.T) {
	was := inputDir
	t.Cleanup(func() { inputDir = was })
	inputDir = filepath.Join(t.TempDir(), "input")
	if err := os.Mkdir(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(inputDir, "event5")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	gone := make(chan []string, 4)
	ended := make(chan error, 1)
	go func() {
		ended <- WatchInputNodes(func() { close(ready) }, func(names []string) { gone <- names })
	}()
	select {
	case <-ready:
	case err := <-ended:
		t.Fatalf("the watch never started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never said it was ready")
	}
	if err := os.Remove(node); err != nil {
		t.Fatal(err)
	}
	select {
	case names := <-gone:
		if !slices.Equal(names, []string{"event5"}) {
			t.Errorf("the deletion named %q, want event5", names)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a node removed after the watch was ready was never seen")
	}
	if err := os.Remove(inputDir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Error("a watch whose directory went away ended with no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a watch whose directory went away kept waiting, and nothing says so")
	}
}
