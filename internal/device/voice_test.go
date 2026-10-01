package device

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRedirectReadsTheHostBack(t *testing.T) {
	defer func(s func(string, string) error, r func(string) (string, error), w time.Duration) {
		setProp, readProp, propWaitFor = s, r, w
	}(setProp, readProp, propWaitFor)
	propWaitFor = 10 * time.Millisecond

	set := map[string]string{}
	setProp = func(name, value string) error { set[name] = value; return nil }
	readProp = func(name string) (string, error) { return set[name], nil }

	if _, err := RedirectSpeech("127.0.0.1", "8443"); err != nil {
		t.Fatalf("RedirectSpeech: %v", err)
	}
	if set[speechHostProp] != "127.0.0.1" || set[speechPortProp] != "8443" {
		t.Errorf("the endpoint is %q:%q, want 127.0.0.1:8443",
			set[speechHostProp], set[speechPortProp])
	}

	// A property that does not take is a Dot that cannot answer, and setprop
	// reports nothing. Both have to be read back: a port that stayed empty
	// sends the app to 127.0.0.1:443, where nothing listens.
	for _, blind := range []string{speechHostProp, speechPortProp} {
		readProp = func(name string) (string, error) {
			if name == blind {
				return "", nil
			}
			return set[name], nil
		}
		if _, err := RedirectSpeech("127.0.0.1", "8443"); err == nil {
			t.Errorf("%s did not read back and was taken for a redirect that worked", blind)
		}
	}
}

// setprop returns before the value is visible to a later getprop. Seen on the
// device: the property file held 127.0.0.1 while the read that followed it came
// back empty, and the relay then refused to start.
func TestRedirectWaitsForAPropertyToSettle(t *testing.T) {
	defer func(s func(string, string) error, r func(string) (string, error), w time.Duration) {
		setProp, readProp, propWaitFor = s, r, w
	}(setProp, readProp, propWaitFor)
	propWaitFor = time.Second

	set := map[string]string{}
	setProp = func(name, value string) error { set[name] = value; return nil }
	blind := 2
	readProp = func(name string) (string, error) {
		if blind > 0 {
			blind--
			return "", nil
		}
		return set[name], nil
	}
	if _, err := RedirectSpeech("127.0.0.1", "8443"); err != nil {
		t.Errorf("RedirectSpeech: %v, want it to wait for the property", err)
	}
}

// A redirect that gives up leaves nothing behind. A host set with no port, or
// either set with no listener, is a Dot that cannot answer at all.
func TestAFailedRedirectClearsWhatItSet(t *testing.T) {
	defer func(s func(string, string) error, r func(string) (string, error), w time.Duration) {
		setProp, readProp, propWaitFor = s, r, w
	}(setProp, readProp, propWaitFor)
	propWaitFor = 10 * time.Millisecond

	set := map[string]string{}
	setProp = func(name, value string) error { set[name] = value; return nil }
	readProp = func(name string) (string, error) {
		if name == speechPortProp {
			return "", nil
		}
		return set[name], nil
	}
	if _, err := RedirectSpeech("127.0.0.1", "8443"); err == nil {
		t.Fatal("a port that never read back was taken for a redirect that worked")
	}
	for _, name := range []string{speechHostProp, speechPortProp} {
		if set[name] != "" {
			t.Errorf("%s is still %q after a failed redirect, so Alexa talks to "+
				"a relay that was never started", name, set[name])
		}
	}
}

func TestReleaseInsistsOnBothProperties(t *testing.T) {
	defer func(s func(string, string) error, r func(string) (string, error)) {
		setProp, readProp = s, r
	}(setProp, readProp)

	set := map[string]string{speechHostProp: "127.0.0.1", speechPortProp: "8443"}
	setProp = func(name, value string) error { set[name] = value; return nil }
	readProp = func(name string) (string, error) { return set[name], nil }

	if err := ReleaseSpeech(); err != nil {
		t.Fatalf("ReleaseSpeech: %v", err)
	}
	for _, name := range []string{speechHostProp, speechPortProp} {
		if set[name] != "" {
			t.Errorf("%s is still %q, so Alexa still talks to a relay that is gone",
				name, set[name])
		}
	}

	set[speechPortProp] = "8443"
	readProp = func(name string) (string, error) {
		if name == speechPortProp {
			return "8443", nil
		}
		return "", nil
	}
	if err := ReleaseSpeech(); err == nil {
		t.Error("a port left set was taken for a release that worked")
	}
}

func TestTrustWritesUnderARemountAndPutsSystemBack(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	var how []string
	remount = func(h string) error { how = append(how, h); return nil }

	path := filepath.Join(t.TempDir(), "cacerts", "9a5ba580.0")
	pem := []byte("-----BEGIN CERTIFICATE-----\n")
	if err := Trust(path, pem); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading what was trusted: %v", err)
	}
	if string(got) != string(pem) {
		t.Errorf("the trust store holds %q, want %q", got, pem)
	}
	if len(how) != 2 || how[0] != "rw" || how[1] != "ro" {
		t.Errorf("remounted %v, want [rw ro]: /system left writable is every later "+
			"write's problem", how)
	}
	if !Trusted(path) {
		t.Error("Trusted says no about a file it just wrote")
	}
}

func TestTrustPutsSystemBackWhenTheWriteFails(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	var how []string
	remount = func(h string) error { how = append(how, h); return nil }

	// A path whose parent is a file, so MkdirAll fails.
	parent := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Trust(filepath.Join(parent, "9a5ba580.0"), []byte("x")); err == nil {
		t.Fatal("writing under a file was taken for a trust that worked")
	}
	if len(how) != 2 || how[1] != "ro" {
		t.Errorf("remounted %v, want /system back at ro after a failed write", how)
	}
}

func TestTrustRefusesWhenSystemWillNotRemount(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	remount = func(string) error { return errors.New("mount: Permission denied") }

	path := filepath.Join(t.TempDir(), "9a5ba580.0")
	if err := Trust(path, []byte("x")); err == nil {
		t.Fatal("a refused remount was taken for a trust that worked")
	}
	if Trusted(path) {
		t.Error("something was written although /system never became writable")
	}
}

func TestUntrustRemovesWhatItNamesAndNothingElse(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	remount = func(string) error { return nil }

	path := filepath.Join(t.TempDir(), "9a5ba580.0")
	if err := Untrust(path); err != nil {
		t.Errorf("Untrust of a path that is not there: %v, want silence", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Untrust(path); err != nil {
		t.Fatalf("Untrust: %v", err)
	}
	if Trusted(path) {
		t.Error("the certificate authority is still in the trust store")
	}
}

func TestSpeechPIDReadsTheProcessName(t *testing.T) {
	defer func(r string) { procRoot = r }(procRoot)
	procRoot = t.TempDir()

	for pid, name := range map[string]string{
		"1":     "/init",
		"951":   speechApp,
		"28904": "amazon.speech.davs.davcservice",
		"29100": speechApp + "ulator",
		"self":  speechApp,
	} {
		dir := filepath.Join(procRoot, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A real cmdline is NUL-terminated, and a prefix of another name must
		// not match: the device runs amazon.speech.davs.davcservice as well.
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(name+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pid, err := speechPID()
	if err != nil {
		t.Fatalf("speechPID: %v", err)
	}
	if pid != 951 {
		t.Errorf("speechPID() = %d, want 951: self is not a number and must not be read "+
			"as one, and neither the davs service nor a longer name beginning the same "+
			"way is this app", pid)
	}
}

func TestRestartSpeechIsSilentWhenTheAppIsNotRunning(t *testing.T) {
	defer func(r string) { procRoot = r }(procRoot)
	procRoot = t.TempDir()
	if err := RestartSpeech(); err != nil {
		t.Errorf("RestartSpeech with no such process: %v, want silence", err)
	}
}

// A daemon that restarts 5 seconds after a panic would kill the speech app on
// every pass, and the app needs about 20 seconds to come back.
func TestRedirectLeavesAnEndpointThatAlreadyPointsHere(t *testing.T) {
	defer func(s func(string, string) error, r func(string) (string, error)) {
		setProp, readProp = s, r
	}(setProp, readProp)

	set := map[string]string{speechHostProp: "127.0.0.1", speechPortProp: "8443"}
	sets := 0
	setProp = func(name, value string) error { sets++; set[name] = value; return nil }
	readProp = func(name string) (string, error) { return set[name], nil }

	moved, err := RedirectSpeech("127.0.0.1", "8443")
	if err != nil {
		t.Fatalf("RedirectSpeech: %v", err)
	}
	if moved {
		t.Error("an endpoint that already points here was reported as moved, so the " +
			"speech app is killed on every start")
	}
	if sets != 0 {
		t.Errorf("setprop ran %d times for an endpoint that needed no change", sets)
	}
}
