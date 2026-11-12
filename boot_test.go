package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type bootLoop struct {
	dir  string
	bin  string
	prev string
	jar  string
}

func newBootLoop(t *testing.T) *bootLoop {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"bin", "map", "tmp", "stubs"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &bootLoop{
		dir:  dir,
		bin:  filepath.Join(dir, "bin", "overdub"),
		prev: filepath.Join(dir, "bin", "overdub.prev"),
		jar:  filepath.Join(dir, "map", "mapdump.jar"),
	}
}

func (b *bootLoop) trial() string  { return filepath.Join(b.dir, "bin", ".overdub-trial") }
func (b *bootLoop) failed() string { return filepath.Join(b.dir, "bin", ".overdub-failed") }

func (b *bootLoop) stub(t *testing.T, label string, limit int, extra string) string {
	t.Helper()
	path := filepath.Join(b.dir, "stubs", label)
	script := "#!/bin/sh\n" +
		"echo \"" + label + " $OVERDUB_ROLLBACK\" >> " + b.dir + "/runs\n" +
		extra + "\n" +
		"if [ \"$(wc -l < " + b.dir + "/runs)\" -ge " + strconv.Itoa(limit) + " ]; then rm -f " + b.bin + "; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func (b *bootLoop) place(t *testing.T, stub, at string) {
	t.Helper()
	data, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (b *bootLoop) write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (b *bootLoop) run(t *testing.T) (runs []string, log string) {
	t.Helper()
	raw, err := os.ReadFile("deploy/overdub.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for _, r := range []struct{ old, new string }{
		{"/data/local/bin", b.dir + "/bin"},
		{"/data/local/map", b.dir + "/map"},
		{"/data/local/tmp", b.dir + "/tmp"},
		{"sleep 5", ":"},
		{"\nNAME=\n", "\nNAME=kitchen\n"},
		{"\n) &\n", "\n)\n"},
	} {
		if !strings.Contains(script, r.old) {
			t.Fatalf("deploy/overdub.sh no longer says %q, which this test rewrites", r.old)
		}
		script = strings.ReplaceAll(script, r.old, r.new)
	}
	path := filepath.Join(b.dir, "overdub.sh")
	b.write(t, path, script)
	if out, err := exec.Command("sh", path).CombinedOutput(); err != nil {
		t.Fatalf("the boot script failed: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(b.dir, "runs"))
	logged, _ := os.ReadFile(filepath.Join(b.dir, "tmp", "overdub.log"))
	return strings.Fields(strings.ReplaceAll(string(data), " 1\n", "\n")), string(logged)
}

func checkRuns(t *testing.T, runs []string, want ...string) {
	t.Helper()
	if strings.Join(runs, " ") != strings.Join(want, " ") {
		t.Errorf("the loop ran %v, want %v", runs, want)
	}
}

func TestAnUpdateThatKeepsExitingRunsThreeTimesThenTheLoopGoesBack(t *testing.T) {
	b := newBootLoop(t)
	b.place(t, b.stub(t, "new", 5, ""), b.bin)
	b.place(t, b.stub(t, "old", 5, ""), b.prev)
	b.write(t, b.trial(), "v2\n")
	b.write(t, b.jar, "new jar")
	b.write(t, b.jar+".prev", "old jar")

	runs, log := b.run(t)
	checkRuns(t, runs, "new", "new", "new", "old", "old")
	if data, _ := os.ReadFile(b.jar); string(data) != "old jar" {
		t.Errorf("the jar holds %q after going back", data)
	}
	for _, path := range []string{b.trial(), b.prev, b.jar + ".prev"} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s outlived the rollback", filepath.Base(path))
		}
	}
	if !strings.Contains(log, "the update to v2 exited 3 times; back to the binary it replaced") {
		t.Errorf("the log does not say the loop went back:\n%s", log)
	}
	if data, _ := os.ReadFile(b.failed()); string(data) != "v2\n" {
		t.Errorf("the failed-update record holds %q, want the version that failed", data)
	}
}

func TestEveryStartSeesTheRollbackFlag(t *testing.T) {
	b := newBootLoop(t)
	b.place(t, b.stub(t, "new", 2, ""), b.bin)
	raw, _ := os.ReadFile("deploy/overdub.sh")
	if !strings.Contains(string(raw), "OVERDUB_ROLLBACK=1 \"$BIN\"") {
		t.Fatal("the loop does not start the daemon with OVERDUB_ROLLBACK=1")
	}
	b.run(t)
	data, _ := os.ReadFile(filepath.Join(b.dir, "runs"))
	if got := string(data); got != "new 1\nnew 1\n" {
		t.Errorf("the daemon saw %q, want OVERDUB_ROLLBACK=1 on every start", got)
	}
}

func TestANewMarkerStartsANewCount(t *testing.T) {
	b := newBootLoop(t)
	v3 := b.stub(t, "v3", 6, "")
	install := "if [ ! -f " + b.dir + "/v2-ran ]; then touch " + b.dir + "/v2-ran; rm -f " + b.trial() +
		"; ln " + b.bin + " " + b.prev + "; echo v3 > " + b.trial() + "; cp " + v3 + " " + b.bin + ".new; mv " +
		b.bin + ".new " + b.bin + "; fi"
	b.place(t, b.stub(t, "v2", 6, install), b.bin)
	b.write(t, b.trial(), "v2\n")

	runs, _ := b.run(t)
	checkRuns(t, runs, "v2", "v3", "v3", "v3", "v2", "v2")
}

func TestAMissingPreviousBinaryIsLoggedAndTheUpdateKept(t *testing.T) {
	b := newBootLoop(t)
	b.place(t, b.stub(t, "new", 5, ""), b.bin)
	b.write(t, b.trial(), "v2\n")
	b.write(t, b.jar, "new jar")
	b.write(t, b.jar+".prev", "old jar")

	runs, log := b.run(t)
	checkRuns(t, runs, "new", "new", "new", "new", "new")
	if !strings.Contains(log, "the update to v2 exited 3 times, and") || !strings.Contains(log, "is missing; keeping it") {
		t.Errorf("the log does not say the previous binary is missing:\n%s", log)
	}
	if data, _ := os.ReadFile(b.jar); string(data) != "new jar" {
		t.Errorf("the jar holds %q; the update was kept, so its jar should be too", data)
	}
	for _, path := range []string{b.jar + ".prev", b.failed()} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s is there after the loop kept the update", filepath.Base(path))
		}
	}
}

func TestWithNoTrialTheLoopNeverGoesBack(t *testing.T) {
	b := newBootLoop(t)
	b.place(t, b.stub(t, "new", 5, ""), b.bin)
	b.place(t, b.stub(t, "old", 5, ""), b.prev)

	runs, _ := b.run(t)
	checkRuns(t, runs, "new", "new", "new", "new", "new")
}

func newTrial(t *testing.T, marker string) trial {
	t.Helper()
	dir := t.TempDir()
	tr := trial{
		marker: filepath.Join(dir, "trial"),
		prev:   filepath.Join(dir, "overdub.prev"),
		jar:    filepath.Join(dir, "mapdump.jar"),
		failed: filepath.Join(dir, "failed"),
	}
	for path, text := range map[string]string{
		tr.marker: marker, tr.prev: "old binary", tr.jar: "new jar", tr.jar + ".prev": "old jar", tr.failed: "v2\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func served() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func TestASettledTrialRemovesTheMarkerAndThePreviousFiles(t *testing.T) {
	tr := newTrial(t, "v2\n")
	tr.settle("v2", served(), time.Millisecond)
	for _, path := range []string{tr.marker, tr.prev, tr.jar + ".prev", tr.failed} {
		if exists(path) {
			t.Errorf("%s outlived a trial that passed", filepath.Base(path))
		}
	}
	if data, _ := os.ReadFile(tr.jar); string(data) != "new jar" {
		t.Errorf("the jar holds %q after a trial that passed", data)
	}
}

func TestATrialWaitsForHomeAssistant(t *testing.T) {
	tr := newTrial(t, "v2\n")
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		tr.settle("v2", ready, time.Millisecond)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if !exists(tr.marker) || !exists(tr.prev) {
		t.Fatal("the trial passed before Home Assistant was served")
	}
	close(ready)
	<-done
	if exists(tr.marker) {
		t.Error("the trial did not pass once Home Assistant was served")
	}
}

func TestATrialLeavesANewerMarkerAlone(t *testing.T) {
	tr := newTrial(t, "v2\n")
	done := make(chan struct{})
	go func() {
		tr.settle("v2", served(), 200*time.Millisecond)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	os.WriteFile(tr.marker, []byte("v3\n"), 0o600)
	<-done
	if data, _ := os.ReadFile(tr.marker); string(data) != "v3\n" {
		t.Errorf("the marker holds %q; the newer update's trial was ended", data)
	}
	if !exists(tr.prev) {
		t.Error("the newer update's previous binary was removed")
	}
}

func TestABinaryUnderAnotherVersionsTrialGoesBackToItsJar(t *testing.T) {
	tr := newTrial(t, "v3\n")
	done := make(chan struct{})
	go func() {
		tr.settle("v2", make(chan struct{}), time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a binary under another version's trial waited for that trial")
	}
	if data, _ := os.ReadFile(tr.jar); string(data) != "old jar" {
		t.Errorf("the jar holds %q, want the one v2 came with", data)
	}
	for _, path := range []string{tr.marker, tr.prev, tr.jar + ".prev"} {
		if exists(path) {
			t.Errorf("%s outlived an install that stopped partway", filepath.Base(path))
		}
	}
	if !exists(tr.failed) {
		t.Error("going back to the jar removed the failed-update record")
	}
}
