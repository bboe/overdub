package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bboe/overdub/internal/esphome"
)

const bootScript = "#!/system/bin/sh\nBIN=/data/local/bin/overdub\n\nNAME=\n\necho start\n"

type fakeRelease struct {
	version string
	binary  []byte
	boot    string
	sumsFor string
	signer  ed25519.PrivateKey
	tamper  bool
	noSig   bool
	pre     bool
	draft   bool
	symlink bool
	hits    *atomic.Int32
	padding int
}

func (f fakeRelease) tarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	dir := "overdub-" + f.version + "/"
	for _, file := range []struct {
		name string
		data []byte
	}{
		{dir + "README.md", []byte("readme")},
		{dir + "build/overdub", f.binary},
		{dir + "deploy/overdub.sh", []byte(f.boot)},
	} {
		if f.symlink && file.name == dir+"build/overdub" {
			if err := tw.WriteHeader(&tar.Header{Name: file.name, Linkname: "/system/bin/sh",
				Typeflag: tar.TypeSymlink}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o755, Size: int64(len(file.data)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(file.data)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func (f fakeRelease) serve(t *testing.T) *httptest.Server {
	t.Helper()
	tarball := f.tarball(t)
	sum := sha256.Sum256(tarball)
	if f.tamper {
		sum[0] ^= 1
	}
	named := f.sumsFor
	if named == "" {
		named = tarballName(f.version)
	}
	sums := []byte(fmt.Sprintf("%x  %s\n", sum, named))
	sig := ed25519.Sign(f.signer, sums)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		if f.hits != nil {
			f.hits.Add(1)
		}
		assets := []map[string]string{
			{"name": tarballName(f.version), "browser_download_url": srv.URL + "/tarball"},
			{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/sums"},
		}
		if !f.noSig {
			assets = append(assets, map[string]string{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/sig"})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": f.version, "body": "## What's new\nthings", "html_url": srv.URL + "/page",
			"prerelease": f.pre, "draft": f.draft, "assets": assets,
			"padding": strings.Repeat(" ", f.padding),
		})
	})
	mux.HandleFunc("/tarball", func(w http.ResponseWriter, _ *http.Request) { w.Write(tarball) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { w.Write(sums) })
	mux.HandleFunc("/sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type dot struct {
	dir       string
	paths     Paths
	mu        sync.Mutex
	states    []esphome.UpdateState
	restarted chan struct{}
}

func newDot(t *testing.T, boot string) *dot {
	t.Helper()
	dir := t.TempDir()
	d := &dot{
		dir: dir,
		paths: Paths{
			Binary: filepath.Join(dir, "overdub"),
			Prev:   filepath.Join(dir, "overdub.prev"),
			Trial:  filepath.Join(dir, ".overdub-trial"),
			Failed: filepath.Join(dir, ".overdub-failed"),
			Boot:   filepath.Join(dir, "overdub.sh"),
		},
		restarted: make(chan struct{}, 1),
	}
	for path, data := range map[string]string{
		d.paths.Binary: "old binary", d.paths.Boot: boot,
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *dot) updater(srv *httptest.Server, key ed25519.PublicKey) *Updater {
	return &Updater{
		Current:  "v0.2.0",
		Name:     "kitchen",
		Key:      key,
		Paths:    d.paths,
		API:      srv.URL + "/latest",
		Client:   srv.Client(),
		Rollback: true,
		Publish: func(s esphome.UpdateState) {
			d.mu.Lock()
			d.states = append(d.states, s)
			d.mu.Unlock()
		},
		Restart: func() { d.restarted <- struct{}{} },
	}
}

func (d *dot) last() esphome.UpdateState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.states) == 0 {
		return esphome.UpdateState{}
	}
	return d.states[len(d.states)-1]
}

func (d *dot) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.states)
}

func (d *dot) read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func installedBoot() string { return strings.Replace(bootScript, "NAME=\n", "NAME=kitchen\n", 1) }

func TestAnUpdateSwapsTheBinaryAndLeavesATrial(t *testing.T) {
	pub, priv := keys(t)
	srv := fakeRelease{version: "v0.3.0", binary: []byte("new binary"),
		boot: bootScript, signer: priv}.serve(t)
	d := newDot(t, installedBoot())
	os.WriteFile(d.paths.Prev, []byte("stale"), 0o755)
	u := d.updater(srv, pub)

	if !u.check() {
		t.Fatal("the check failed")
	}
	if s := d.last(); s.Latest != "v0.3.0" || s.Current != "v0.2.0" ||
		!strings.Contains(s.Notes, "What's new") || s.URL != srv.URL+"/page" {
		t.Fatalf("after the check Home Assistant was told %+v", s)
	}
	defer syscall.Umask(syscall.Umask(0o077))
	if err := u.install(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.restarted:
	default:
		t.Fatal("the daemon was not restarted into the update")
	}
	for path, want := range map[string]string{
		d.paths.Binary:          "new binary",
		d.paths.Prev:            "old binary",
		d.paths.Trial:           "v0.3.0\n",
		d.paths.Binary + ".new": "<open " + d.paths.Binary + ".new: no such file or directory>",
	} {
		if got := d.read(t, path); got != want {
			t.Errorf("%s holds %q, want %q", filepath.Base(path), got, want)
		}
	}
	if info, err := os.Stat(d.paths.Binary); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("the binary's mode is %v (%v), want 0755", info.Mode(), err)
	}
	if s := d.last(); !s.InProgress || !s.HasProgress || s.Progress != 100 {
		t.Errorf("the last state before the restart was %+v", s)
	}
}

func TestAnUpdateIsRefused(t *testing.T) {
	pub, priv := keys(t)
	_, other := keys(t)
	good := fakeRelease{version: "v0.3.0", binary: []byte("new binary"),
		boot: bootScript, signer: priv}
	for _, c := range []struct {
		name     string
		release  func(fakeRelease) fakeRelease
		boot     string
		rollback bool
		want     string
	}{
		{"signed by another key", func(f fakeRelease) fakeRelease { f.signer = other; return f },
			installedBoot(), true, "does not check against the release key"},
		{"whose tarball does not match", func(f fakeRelease) fakeRelease { f.tamper = true; return f },
			installedBoot(), true, "the tarball's sha256"},
		{"whose SHA256SUMS names another version",
			func(f fakeRelease) fakeRelease { f.sumsFor = tarballName("v0.2.0"); return f },
			installedBoot(), true, "does not name overdub-v0.3.0.tar.gz"},
		{"that changes the boot script", func(f fakeRelease) fakeRelease { return f },
			bootScript + "# older\n", true, "changes the boot script"},
		{"under a loop that cannot go back", func(f fakeRelease) fakeRelease { return f },
			installedBoot(), false, "cannot go back"},
		{"whose boot script names NAME twice",
			func(f fakeRelease) fakeRelease { f.boot = bootScript + "NAME=\n"; return f },
			installedBoot(), true, "no single NAME= line"},
		{"whose binary is a symlink, so empty", func(f fakeRelease) fakeRelease { f.symlink = true; return f },
			installedBoot(), true, "has no overdub-v0.3.0/build/overdub"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := c.release(good).serve(t)
			d := newDot(t, c.boot)
			u := d.updater(srv, pub)
			u.Rollback = c.rollback
			if !u.check() {
				t.Fatal("the check failed")
			}
			err := u.install()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("install gave %v, want an error saying %q", err, c.want)
			}
			if got := d.read(t, d.paths.Binary); got != "old binary" {
				t.Errorf("the binary holds %q after a refused update", got)
			}
			for _, path := range []string{d.paths.Trial, d.paths.Prev, d.paths.Binary + ".new"} {
				if _, err := os.Stat(path); err == nil {
					t.Errorf("%s exists after a refused update", filepath.Base(path))
				}
			}
			select {
			case <-d.restarted:
				t.Error("a refused update restarted the daemon")
			default:
			}
		})
	}
}

func TestAFailedInstallIsShownInTheNotesAndClearsProgress(t *testing.T) {
	pub, _ := keys(t)
	_, other := keys(t)
	srv := fakeRelease{version: "v0.3.0", binary: []byte("b"), boot: bootScript,
		signer: other}.serve(t)
	d := newDot(t, installedBoot())
	u := d.updater(srv, pub)
	u.Start(time.Hour, time.Hour, time.Hour)
	u.checks <- struct{}{}
	waitFor(t, func() bool { return d.count() > 0 })
	u.Install()
	waitFor(t, func() bool { return strings.Contains(d.last().Notes, "The last install failed") })
	if s := d.last(); s.InProgress || s.Latest != "v0.3.0" {
		t.Errorf("after a failed install Home Assistant was told %+v", s)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAReleaseIsNotOffered(t *testing.T) {
	pub, priv := keys(t)
	for _, c := range []struct {
		name    string
		release fakeRelease
	}{
		{"that is not newer", fakeRelease{version: "v0.2.0", signer: priv}},
		{"that is older", fakeRelease{version: "v0.1.9", signer: priv}},
		{"that is unsigned", fakeRelease{version: "v0.3.0", signer: priv, noSig: true}},
		{"that is a prerelease", fakeRelease{version: "v0.3.0-rc1", signer: priv, pre: true}},
		{"that is marked a prerelease", fakeRelease{version: "v0.3.0", signer: priv, pre: true}},
		{"whose tag is not stable", fakeRelease{version: "v0.3.0-rc1", signer: priv}},
		{"that is a draft", fakeRelease{version: "v0.3.0", signer: priv, draft: true}},
		{"whose answer is too long", fakeRelease{version: "v0.3.0", signer: priv, padding: maxAPI}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.release.boot = bootScript
			srv := c.release.serve(t)
			d := newDot(t, installedBoot())
			u := d.updater(srv, pub)
			u.Start(time.Hour, time.Hour, time.Hour)
			u.check()
			if s := d.last(); d.count() > 0 && (s.Latest != "v0.2.0" || s.Notes != "") {
				t.Errorf("Home Assistant was offered %+v", s)
			}
			if err := u.install(); err != nil {
				t.Errorf("install of a release not offered gave %v", err)
			}
			if got := d.read(t, d.paths.Binary); got != "old binary" {
				t.Errorf("the binary holds %q", got)
			}
		})
	}
}

func TestNothingIsPublishedBeforeACheckSucceeds(t *testing.T) {
	pub, _ := keys(t)
	d := newDot(t, installedBoot())
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	u := d.updater(srv, pub)
	u.Start(time.Hour, time.Hour, time.Hour)
	if u.check() || d.count() != 0 {
		t.Errorf("before a check succeeded Home Assistant was told %+v", d.last())
	}
}

func TestABinaryOnItsOwnTrialTellsHomeAssistantAtOnce(t *testing.T) {
	pub, _ := keys(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	for marker, want := range map[string]int{"v0.2.0\n": 1, "v0.3.0\n": 0, "": 0} {
		d := newDot(t, installedBoot())
		if marker != "" {
			os.WriteFile(d.paths.Trial, []byte(marker), 0o600)
		}
		u := d.updater(srv, pub)
		u.Start(time.Hour, time.Hour, time.Hour)
		if d.count() != want {
			t.Errorf("under the marker %q, start published %d states, want %d", marker, d.count(), want)
			continue
		}
		if s := d.last(); want == 1 && (s.Current != "v0.2.0" || s.Latest != "v0.2.0" || s.InProgress) {
			t.Errorf("under its own trial, start published %+v", s)
		}
	}
}

func TestABinaryTheLoopWentBackToNamesTheFailedReleaseAtOnce(t *testing.T) {
	pub, _ := keys(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	for failed, want := range map[string]int{"v0.3.0\n": 1, "v0.1.0\n": 0, "v0.3.0-rc1\n": 0, "": 0} {
		d := newDot(t, installedBoot())
		if failed != "" {
			os.WriteFile(d.paths.Failed, []byte(failed), 0o644)
		}
		u := d.updater(srv, pub)
		u.Start(time.Hour, time.Hour, time.Hour)
		if d.count() != want {
			t.Errorf("with the record %q, start published %d states, want %d", failed, d.count(), want)
			continue
		}
		s := d.last()
		if want == 1 && (s.Current != "v0.2.0" || s.Latest != "v0.3.0" || s.InProgress ||
			!strings.Contains(s.Notes, "v0.3.0 exited 3 times after it was installed")) {
			t.Errorf("after going back, start published %+v", s)
		}
	}
}

func TestAReleaseThatFailedIsNamedInTheNotes(t *testing.T) {
	pub, priv := keys(t)
	srv := fakeRelease{version: "v0.3.0", binary: []byte("b"), boot: bootScript,
		signer: priv}.serve(t)
	for failed, named := range map[string]bool{"v0.3.0\n": true, "v0.2.9\n": false, "": false} {
		d := newDot(t, installedBoot())
		if failed != "" {
			os.WriteFile(d.paths.Failed, []byte(failed), 0o644)
		}
		u := d.updater(srv, pub)
		if !u.check() {
			t.Fatal("the check failed")
		}
		said := strings.Contains(d.last().Notes, "v0.3.0 exited 3 times after it was installed, so the Dot went back to v0.2.0")
		if said != named {
			t.Errorf("with the record %q, the notes were %q", failed, d.last().Notes)
		}
	}
}

func TestAnInstallWaitsForTheRunningTrial(t *testing.T) {
	pub, priv := keys(t)
	srv := fakeRelease{version: "v0.3.0", binary: []byte("new binary"),
		boot: bootScript, signer: priv}.serve(t)
	d := newDot(t, installedBoot())
	os.WriteFile(d.paths.Trial, []byte("v0.2.0\n"), 0o600)
	u := d.updater(srv, pub)
	u.check()
	if err := u.install(); err == nil || !strings.Contains(err.Error(), "still on trial") {
		t.Fatalf("install during a trial gave %v", err)
	}
	if got := d.read(t, d.paths.Trial); got != "v0.2.0\n" {
		t.Errorf("the running trial's marker holds %q", got)
	}
	if got := d.read(t, d.paths.Binary); got != "old binary" {
		t.Errorf("the binary holds %q", got)
	}

	os.Remove(d.paths.Trial)
	u.mu.Lock()
	u.failure = "an earlier failure"
	u.mu.Unlock()
	if err := u.install(); err != nil {
		t.Fatal(err)
	}
	if s := d.last(); strings.Contains(s.Notes, "earlier failure") {
		t.Errorf("a successful install still showed the last failure: %q", s.Notes)
	}
}

func TestACheckWithinAMinuteOfTheLastIsDropped(t *testing.T) {
	pub, priv := keys(t)
	var hits atomic.Int32
	srv := fakeRelease{version: "v0.3.0", boot: bootScript, signer: priv, hits: &hits}.serve(t)
	d := newDot(t, installedBoot())
	u := d.updater(srv, pub)
	u.Start(time.Hour, time.Hour, time.Hour)
	u.Check()
	waitFor(t, func() bool { return hits.Load() == 1 && d.count() > 0 })
	u.Check()
	time.Sleep(200 * time.Millisecond)
	if n := hits.Load(); n != 1 {
		t.Errorf("2 checks a moment apart asked GitHub %d times", n)
	}
}

func TestAnErrorIsCutAndMadeValidUTF8(t *testing.T) {
	got := plain(fmt.Errorf("bad \xff %s", strings.Repeat("x", 1000)))
	if !utf8.ValidString(got) || len(got) > maxError+len("…") || !strings.HasPrefix(got, "bad ?") {
		t.Errorf("plain gave %q", got)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.3.0", "v0.2.0", true},
		{"v0.2.1", "v0.2.0", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v0.3.0", "v0.3.0-rc1", true},
		{"v0.3.0", "v0.3.1-rc1", false},
		{"v0.3.0", "unversioned", false},
		{"0.3.0", "v0.2.0", false},
		{"v0.3", "v0.2.0", false},
		{"v0.03.0", "v0.2.0", false},
		{"v0.3.0-", "v0.2.0", false},
	} {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestCutKeepsUTF8Whole(t *testing.T) {
	if got := cut("aé", 2); got != "a…" {
		t.Errorf("cut split a rune: %q", got)
	}
	if got := cut("abc", 3); got != "abc" {
		t.Errorf("cut changed a short string: %q", got)
	}
}
