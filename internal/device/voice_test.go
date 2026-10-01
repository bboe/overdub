package device

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func fakeEndpoint(t *testing.T, now string) *string {
	t.Helper()
	oldRead, oldWrite, oldWait := readEndpoint, writeEndpoint, propWaitFor
	oldPuffinWait, oldPuffinPoll := puffinWaitFor, puffinPoll
	t.Cleanup(func() {
		readEndpoint, writeEndpoint, propWaitFor = oldRead, oldWrite, oldWait
		puffinWaitFor, puffinPoll = oldPuffinWait, oldPuffinPoll
	})
	propWaitFor = 50 * time.Millisecond
	puffinWaitFor, puffinPoll = 50*time.Millisecond, time.Millisecond
	readEndpoint = func() (string, error) { return now, nil }
	writeEndpoint = func(url string) error { now = url; return nil }
	return &now
}

func TestPointSpeechWritesAndReadsBack(t *testing.T) {
	now := fakeEndpoint(t, "https://avs-alexa-14-na.amazon.com")
	if err := PointSpeech("https://127.0.0.1:8443"); err != nil {
		t.Fatal(err)
	}
	if *now != "https://127.0.0.1:8443" {
		t.Errorf("the endpoint is %q, want the relay", *now)
	}
}

func TestPointSpeechWaitsForTheWriteToSettle(t *testing.T) {
	now := fakeEndpoint(t, "https://avs-alexa-14-na.amazon.com")
	propWaitFor = time.Second
	stale := 2
	readEndpoint = func() (string, error) {
		if stale > 0 {
			stale--
			return "https://avs-alexa-14-na.amazon.com", nil
		}
		return *now, nil
	}
	if err := PointSpeech("https://127.0.0.1:8443"); err != nil {
		t.Errorf("a write that settled after 2 reads was refused: %v", err)
	}
}

func TestAReadThatFailsWhileAlexaReconnectsIsRetried(t *testing.T) {
	now := fakeEndpoint(t, "https://avs-alexa-14-na.amazon.com")
	propWaitFor = time.Second
	busy := 2
	readEndpoint = func() (string, error) {
		if *now == "https://127.0.0.1:8443" && busy > 0 {
			busy--
			return "", errors.New("signal: killed")
		}
		return *now, nil
	}
	if err := PointSpeech("https://127.0.0.1:8443"); err != nil {
		t.Errorf("a read-back that failed twice while PuffinApp reconnected ended the switch: %v", err)
	}
}

func TestAWriteThatNeverLandsIsAnError(t *testing.T) {
	fakeEndpoint(t, "https://avs-alexa-14-na.amazon.com")
	writeEndpoint = func(string) error { return nil }
	if err := PointSpeech("https://127.0.0.1:8443"); err == nil {
		t.Error("lipc-set-prop exits 0 for a write that did not land, and that was taken" +
			" for a redirect that worked")
	}
}

func TestPointSpeechLeavesAnEndpointAlreadyThere(t *testing.T) {
	fakeEndpoint(t, "https://127.0.0.1:8443")
	writeEndpoint = func(string) error {
		t.Error("the endpoint was written although it already pointed there; each write drops" +
			" Alexa's session")
		return nil
	}
	if err := PointSpeech("https://127.0.0.1:8443"); err != nil {
		t.Fatal(err)
	}
}

func fakeSDKConfig(t *testing.T, body string) {
	t.Helper()
	old := sdkConfigPath
	t.Cleanup(func() { sdkConfigPath = old })
	sdkConfigPath = filepath.Join(t.TempDir(), "AlexaClientSDKConfig.json")
	if err := os.WriteFile(sdkConfigPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sdkConfig = `{
	"acl": {
		"endpoint": "https://avs-alexa-14-na.amazon.com"
	},
	"externalMediaPlayer": {
        // Agent identifier provided by AlexaMusic.
		"agentString": "X"
	}
}`

func TestTheUpstreamIsWhereAlexaPointsNow(t *testing.T) {
	fakeEndpoint(t, "https://avs-alexa-4-na.amazon.com")
	fakeSDKConfig(t, sdkConfig)
	if got, err := SpeechUpstream("https://127.0.0.1:8443"); err != nil || got != "https://avs-alexa-4-na.amazon.com" {
		t.Errorf("upstream = %q, %v; want where Alexa points now: Amazon may have moved her", got, err)
	}
}

func TestAnUpstreamLeftOnTheRelayFallsBackToTheStockEndpoint(t *testing.T) {
	fakeEndpoint(t, "https://127.0.0.1:8443")
	fakeSDKConfig(t, sdkConfig)
	if got, err := SpeechUpstream("https://127.0.0.1:8443"); err != nil || got != "https://avs-alexa-14-na.amazon.com" {
		t.Errorf("upstream = %q, %v; want the stock endpoint: a daemon that died left Alexa on"+
			" the relay, and the relay must not forward to itself", got, err)
	}
}

func TestARelayThatDidNotStartGivesAlexaBackTheStockEndpoint(t *testing.T) {
	now := fakeEndpoint(t, "https://127.0.0.1:8443")
	fakeSDKConfig(t, sdkConfig)
	if err := LeaveSpeech("https://127.0.0.1:8443"); err != nil {
		t.Fatal(err)
	}
	if *now != "https://avs-alexa-14-na.amazon.com" {
		t.Errorf("the endpoint is %q; a daemon before this one left Alexa on the relay, and"+
			" nothing listens there now", *now)
	}
}

func TestTheUpstreamWaitsForPuffinAppToAnswer(t *testing.T) {
	fakeEndpoint(t, "https://avs-alexa-4-na.amazon.com")
	fakeSDKConfig(t, sdkConfig)
	puffinWaitFor = time.Second
	starting := 3
	read := readEndpoint
	readEndpoint = func() (string, error) {
		if starting > 0 {
			starting--
			return "", errors.New("exit status 3")
		}
		return read()
	}
	if got, err := SpeechUpstream("https://127.0.0.1:8443"); err != nil || got != "https://avs-alexa-4-na.amazon.com" {
		t.Errorf("upstream = %q, %v; at boot PuffinApp answers LIPC seconds after the daemon"+
			" starts, and giving up left Alexa on a relay that never started", got, err)
	}
}

func TestLeaveSpeechLeavesAnEndpointThatIsNotOurs(t *testing.T) {
	fakeEndpoint(t, "https://avs-alexa-4-na.amazon.com")
	fakeSDKConfig(t, sdkConfig)
	writeEndpoint = func(string) error {
		t.Error("an endpoint that was never the relay's was written; each write drops Alexa's session")
		return nil
	}
	if err := LeaveSpeech("https://127.0.0.1:8443"); err != nil {
		t.Fatal(err)
	}
}

func TestAStockConfigWithNoEndpointIsAnError(t *testing.T) {
	fakeSDKConfig(t, `{"acl": {}}`)
	if got, err := StockSpeechEndpoint(); err == nil {
		t.Errorf("a config with no endpoint read as %q", got)
	}
}

func TestTrustWritesEveryPathUnderOneRemount(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	defer syscall.Umask(syscall.Umask(0o077))
	var how []string
	remount = func(h string) error { how = append(how, h); return nil }

	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "cacerts", "4c55d173.0"), filepath.Join(dir, "cacerts-sha1", "cbac1ab7.0")}
	pem := []byte("-----BEGIN CERTIFICATE-----\n")
	if err := Trust(paths, pem); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	for _, path := range paths {
		if got, err := os.ReadFile(path); err != nil || string(got) != string(pem) {
			t.Errorf("%s holds %q, %v; want the certificate", path, got, err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("%s has mode %v, %v; want 0644 whatever the umask: the daemon's is 077", path, info.Mode().Perm(), err)
		}
	}
	if !slices.Equal(how, []string{"rw", "ro"}) {
		t.Errorf("remounted %v, want [rw ro]: / left writable is every later write's problem", how)
	}
}

func TestAStaleOrUnreadableCertificateIsNotTrusted(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022))
	path := filepath.Join(t.TempDir(), "4c55d173.0")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Trusted(path, []byte("new")) {
		t.Error("a certificate from an earlier identity was taken as this one; it has the same" +
			" name, and Alexa would refuse the relay")
	}
	if !Trusted(path, []byte("old")) {
		t.Error("the certificate as written was not trusted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if Trusted(path, []byte("old")) {
		t.Error("a 0600 certificate was taken as trusted")
	}
}

func TestTrustPutsTheRootBackWhenAWriteFails(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	var how []string
	remount = func(h string) error { how = append(how, h); return nil }

	parent := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Trust([]string{filepath.Join(parent, "4c55d173.0")}, []byte("x")); err == nil {
		t.Fatal("writing under a file was taken for a trust that worked")
	}
	if !slices.Equal(how, []string{"rw", "ro"}) {
		t.Errorf("remounted %v, want / back at ro after a failed write", how)
	}
}

func TestTrustRefusesWhenTheRootWillNotRemount(t *testing.T) {
	defer func(r func(string) error) { remount = r }(remount)
	remount = func(string) error { return errors.New("mount: Permission denied") }

	path := filepath.Join(t.TempDir(), "4c55d173.0")
	if err := Trust([]string{path}, []byte("x")); err == nil {
		t.Fatal("a refused remount was taken for a trust that worked")
	}
	if Trusted(path, []byte("x")) {
		t.Error("something was written although / never became writable")
	}
}
