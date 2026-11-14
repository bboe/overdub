// Package update offers Home Assistant the latest overdub release and installs
// it: it reads GitHub's latest release, checks SHA256SUMS against the release
// key, unpacks the binary, and leaves it for the boot script to try.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bboe/overdub/internal/esphome"
)

const (
	maxAPI     = 1 << 20
	maxSums    = 4 << 10
	maxTarball = 64 << 20
	maxFile    = 48 << 20
	maxNotes   = 8 << 10
	maxError   = 300

	checkGap = time.Minute

	title = "overdub"
)

type Paths struct {
	Binary string
	Prev   string
	Trial  string
	Failed string
	Boot   string
}

type Updater struct {
	Current  string
	Name     string
	Key      ed25519.PublicKey
	Paths    Paths
	API      string
	Client   *http.Client
	Rollback bool
	Publish  func(esphome.UpdateState)
	Restart  func()

	checks   chan struct{}
	installs chan struct{}

	mu        sync.Mutex
	release   release
	known     bool
	failure   string
	lastCheck time.Time
}

type release struct {
	version string
	notes   string
	url     string
	tarball string
	sums    string
	sig     string
}

func (u *Updater) Check() {
	select {
	case u.checks <- struct{}{}:
	default:
	}
}

func (u *Updater) Install() {
	select {
	case u.installs <- struct{}{}:
	default:
	}
}

func (u *Updater) Start(first, every, retry time.Duration) {
	u.checks = make(chan struct{}, 1)
	u.installs = make(chan struct{}, 1)
	failed, _ := os.ReadFile(u.Paths.Failed)
	marked, _ := os.ReadFile(u.Paths.Trial)
	if v := strings.TrimSuffix(string(failed), "\n"); stable.MatchString(v) && Newer(v, u.Current) {
		u.release = release{version: v}
		u.publish(false, 0)
	} else if string(marked) == u.Current+"\n" {
		u.publish(false, 0)
	}
	go u.run(first, every, retry)
}

func (u *Updater) run(first, every, retry time.Duration) {
	timer := time.NewTimer(first)
	for {
		select {
		case <-timer.C:
			next := every
			if !u.check() {
				next = retry
			}
			timer.Reset(next)
		case <-u.checks:
			u.mu.Lock()
			recent := !u.lastCheck.IsZero() && time.Since(u.lastCheck) < checkGap
			u.mu.Unlock()
			if !recent {
				u.check()
			}
		case <-u.installs:
			if err := u.install(); err != nil {
				failure := plain(err)
				log.Printf("update: install failed: %s", failure)
				u.mu.Lock()
				u.failure = failure
				u.mu.Unlock()
				u.publish(false, 0)
			}
		}
	}
}

func (u *Updater) check() bool {
	u.mu.Lock()
	u.lastCheck = time.Now()
	u.mu.Unlock()
	r, err := u.latest()
	if err != nil {
		log.Printf("update: checking for a release: %s", plain(err))
		return false
	}
	u.mu.Lock()
	changed := !u.known || r != u.release
	u.release, u.known = r, true
	u.mu.Unlock()
	if changed {
		if Newer(r.version, u.Current) {
			log.Printf("update: %s is available; this is %s", r.version, u.Current)
		} else {
			log.Printf("update: %s is the latest release; this is %s", r.version, u.Current)
		}
	}
	u.publish(false, 0)
	return true
}

func (u *Updater) publish(inProgress bool, progress float32) {
	u.mu.Lock()
	r, failure := u.release, u.failure
	u.mu.Unlock()
	state := esphome.UpdateState{
		Current: u.Current, Latest: u.Current, Title: title,
		InProgress: inProgress, HasProgress: inProgress, Progress: progress,
	}
	if Newer(r.version, u.Current) {
		state.Latest, state.Notes, state.URL = r.version, r.notes, r.url
		if failed, err := os.ReadFile(u.Paths.Failed); err == nil && string(failed) == r.version+"\n" {
			state.Notes = cut("**"+r.version+" exited 3 times after it was installed, so the Dot went back to "+
				u.Current+".**\n\n"+state.Notes, maxNotes)
		}
	}
	if failure != "" {
		state.Notes = cut("**The last install failed:** "+failure+"\n\n"+state.Notes, maxNotes)
	}
	u.Publish(state)
}

var stable = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func (u *Updater) latest() (release, error) {
	body, err := u.get(u.API, maxAPI, "application/vnd.github+json", nil)
	if err != nil {
		return release{}, err
	}
	var answer struct {
		TagName    string `json:"tag_name"`
		Body       string `json:"body"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return release{}, fmt.Errorf("the release does not parse: %w", err)
	}
	if answer.Draft || answer.Prerelease || !stable.MatchString(answer.TagName) {
		return release{}, fmt.Errorf("the latest release is %.40q, not a stable vX.Y.Z", answer.TagName)
	}
	r := release{version: answer.TagName, notes: cut(answer.Body, maxNotes), url: answer.HTMLURL}
	for _, a := range answer.Assets {
		switch a.Name {
		case tarballName(r.version):
			r.tarball = a.URL
		case "SHA256SUMS":
			r.sums = a.URL
		case "SHA256SUMS.sig":
			r.sig = a.URL
		}
	}
	if r.tarball == "" || r.sums == "" || r.sig == "" {
		return release{}, fmt.Errorf("%s lacks its tarball, SHA256SUMS or SHA256SUMS.sig", r.version)
	}
	return r, nil
}

func tarballName(version string) string { return "overdub-" + version + ".tar.gz" }

func (u *Updater) install() error {
	u.mu.Lock()
	r, known := u.release, u.known
	u.failure = ""
	u.mu.Unlock()
	if !known || !Newer(r.version, u.Current) {
		return nil
	}
	if _, err := os.Stat(u.Paths.Trial); err == nil {
		return errors.New("the running binary is still on trial; install again once Home Assistant" +
			" has been connected to it for a minute")
	}
	if !u.Rollback {
		return errors.New("the boot script's loop cannot go back to this binary if the update" +
			" fails; run install.py from this release and reboot the Dot once")
	}
	log.Printf("update: installing %s", r.version)
	u.publish(true, 0)

	sums, err := u.get(r.sums, maxSums, "", nil)
	if err != nil {
		return err
	}
	sig, err := u.get(r.sig, ed25519.SignatureSize, "", nil)
	if err != nil {
		return err
	}
	if !ed25519.Verify(u.Key, sums, sig) {
		return errors.New("SHA256SUMS.sig does not check against the release key")
	}
	want, err := sumFor(sums, tarballName(r.version))
	if err != nil {
		return err
	}

	tarball, err := u.get(r.tarball, maxTarball, "", func(p float32) { u.publish(true, p*90) })
	if err != nil {
		return err
	}
	if got := sha256.Sum256(tarball); !bytes.Equal(got[:], want) {
		return fmt.Errorf("the tarball's sha256 is %x; SHA256SUMS says %x", got, want)
	}
	files, err := unpack(tarball, "overdub-"+r.version+"/")
	if err != nil {
		return err
	}
	if err := u.checkBoot(files.boot); err != nil {
		return err
	}
	u.publish(true, 95)
	if err := u.swap(files, r.version); err != nil {
		return err
	}
	log.Printf("update: installed %s; restarting into it", r.version)
	u.publish(true, 100)
	u.Restart()
	return nil
}

func sumFor(sums []byte, name string) ([]byte, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		hash, file, ok := strings.Cut(line, "  ")
		if !ok || strings.TrimPrefix(file, "*") != name {
			continue
		}
		sum, err := hex.DecodeString(hash)
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("SHA256SUMS has no sha256 for %s", name)
		}
		return sum, nil
	}
	return nil, fmt.Errorf("SHA256SUMS does not name %s", name)
}

func (u *Updater) get(url string, limit int64, accept string, progress func(float32)) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "overdub/"+u.Current)
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%s is %d bytes, over %d", req.URL.Path, resp.ContentLength, limit)
	}
	var out bytes.Buffer
	buf := make([]byte, 64<<10)
	last := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		out.Write(buf[:n])
		if int64(out.Len()) > limit {
			return nil, fmt.Errorf("%s sent more than %d bytes", req.URL.Path, limit)
		}
		if progress != nil && resp.ContentLength > 0 && time.Since(last) >= time.Second {
			last = time.Now()
			progress(float32(out.Len()) / float32(resp.ContentLength))
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

type unpacked struct {
	binary []byte
	boot   []byte
}

func unpack(tarball []byte, prefix string) (unpacked, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return unpacked{}, err
	}
	var files unpacked
	want := map[string]*[]byte{
		prefix + "build/overdub":     &files.binary,
		prefix + "deploy/overdub.sh": &files.boot,
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return unpacked{}, err
		}
		dst, ok := want[h.Name]
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxFile {
			return unpacked{}, fmt.Errorf("%s is %d bytes, over %d", h.Name, h.Size, maxFile)
		}
		if *dst, err = io.ReadAll(tr); err != nil {
			return unpacked{}, err
		}
	}
	for name, dst := range want {
		if len(*dst) == 0 {
			return unpacked{}, fmt.Errorf("the tarball has no %s", name)
		}
	}
	return files, nil
}

var nameLine = regexp.MustCompile(`(?m)^NAME=$`)

func (u *Updater) checkBoot(boot []byte) error {
	if len(nameLine.FindAllIndex(boot, -1)) != 1 {
		return errors.New("the release's boot script has no single NAME= line")
	}
	want := nameLine.ReplaceAll(boot, []byte("NAME="+u.Name))
	have, err := os.ReadFile(u.Paths.Boot)
	if err != nil {
		return err
	}
	if !bytes.Equal(have, want) {
		return errors.New("this release changes the boot script, which an update does not" +
			" replace; run its install.py instead")
	}
	return nil
}

func (u *Updater) swap(files unpacked, version string) error {
	p := u.Paths
	newBinary := p.Binary + ".new"
	if err := writeFile(newBinary, files.binary, 0o755); err != nil {
		return err
	}

	undo := func(err error) error {
		os.Remove(newBinary)
		os.Remove(p.Trial)
		return err
	}
	if err := os.Remove(p.Prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return undo(err)
	}
	if err := os.Link(p.Binary, p.Prev); err != nil {
		return undo(err)
	}
	if err := writeFile(p.Trial, []byte(version+"\n"), 0o600); err != nil {
		return undo(err)
	}
	if err := os.Rename(newBinary, p.Binary); err != nil {
		return undo(err)
	}
	return nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	written, err := os.ReadFile(path)
	if err == nil && !bytes.Equal(written, data) {
		err = fmt.Errorf("%s reads back different from what was written", path)
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func Newer(latest, current string) bool {
	l, lok := parse(latest)
	c, cok := parse(current)
	if !lok || !cok {
		return false
	}
	for i := range l.core {
		if l.core[i] != c.core[i] {
			return l.core[i] > c.core[i]
		}
	}
	return c.pre && !l.pre
}

type semver struct {
	core [3]int
	pre  bool
}

func parse(v string) (semver, bool) {
	rest, ok := strings.CutPrefix(v, "v")
	if !ok {
		return semver{}, false
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 || (hasPre && pre == "") {
		return semver{}, false
	}
	var out semver
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || part != strconv.Itoa(n) {
			return semver{}, false
		}
		out.core[i] = n
	}
	out.pre = hasPre
	return out, true
}

func plain(err error) string {
	return cut(strings.ToValidUTF8(err.Error(), "?"), maxError)
}

func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
