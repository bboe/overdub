package clip

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fixtureFrames = 24000
	fixtureSHA256 = "e422bbdefcd04091d7ac5b70413a9c9fe7c90a33a2852c5dd763f3c4fae79423"
)

var errStopped = errors.New("stopped")

type fakeFeed struct {
	done func()

	mu       sync.Mutex
	pcm      []int16
	finished bool
	stopped  bool
	stuck    bool
	hold     chan struct{}
}

func (f *fakeFeed) Write(pcm []int16) error {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return errStopped
	}
	f.pcm = append(f.pcm, pcm...)
	return nil
}

func (f *fakeFeed) Finish() {
	f.mu.Lock()
	f.finished = true
	stuck := f.stuck
	f.mu.Unlock()
	if !stuck {
		go f.done()
	}
}

func (f *fakeFeed) Stop() {
	f.mu.Lock()
	first := !f.stopped
	f.stopped = true
	f.mu.Unlock()
	if first {
		go f.done()
	}
}

type event struct {
	start        bool
	announcement bool
	err          error
}

type rig struct {
	player *Player
	events chan event
	feeds  chan *fakeFeed
	hold   chan struct{}
	stuck  bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{events: make(chan event, 16), feeds: make(chan *fakeFeed, 4)}
	r.player = &Player{
		Client: &http.Client{},
		Open: func(rate int, done func()) (Feed, error) {
			if rate != Rate {
				t.Errorf("a feed opened at %d Hz, want %d", rate, Rate)
			}
			f := &fakeFeed{done: done, hold: r.hold, stuck: r.stuck}
			r.feeds <- f
			return f, nil
		},
		OnStart: func(a bool) { r.events <- event{start: true, announcement: a} },
		OnEnd:   func(a bool, err error) { r.events <- event{announcement: a, err: err} },
	}
	return r
}

func (r *rig) next(t *testing.T) event {
	t.Helper()
	select {
	case e := <-r.events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no start or end was reported")
		return event{}
	}
}

func (r *rig) quiet(t *testing.T) {
	t.Helper()
	select {
	case e := <-r.events:
		t.Fatalf("reported %+v, want nothing more", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/clip.flac")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serve(t *testing.T, body []byte, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/esphome/ffmpeg_proxy/x/y.flac"
}

func TestAClipFromTheProxyDecodesToWhatFFmpegDecodes(t *testing.T) {
	r := newRig(t)
	if err := r.player.Play(serve(t, fixture(t), http.StatusOK), true); err != nil {
		t.Fatal(err)
	}
	if e := r.next(t); !e.start || !e.announcement {
		t.Fatalf("first report %+v, want the announcement starting", e)
	}
	if e := r.next(t); e.start || e.err != nil || !e.announcement {
		t.Fatalf("second report %+v, want the announcement ending cleanly", e)
	}
	f := <-r.feeds
	if !f.finished {
		t.Error("the feed was never told the clip had ended, so the mixer waits for more")
	}
	if got := len(f.pcm) / Channels; got != fixtureFrames {
		t.Fatalf("decoded %d frames, want %d", got, fixtureFrames)
	}
	var pcm bytes.Buffer
	for _, v := range f.pcm {
		binary.Write(&pcm, binary.LittleEndian, v)
	}
	if sum := sha256.Sum256(pcm.Bytes()); hex.EncodeToString(sum[:]) != fixtureSHA256 {
		t.Error("the decoded samples differ from ffmpeg's; FLAC is lossless, so they must not")
	}
}

func TestAFailedFetchIsReportedAsTheEnd(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   []byte
		status int
		want   string
	}{
		{"a 404", nil, http.StatusNotFound, "HTTP 404"},
		{"an mp3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), http.StatusOK, "not FLAC"},
		{"an empty reply", nil, http.StatusOK, "not FLAC"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			if err := r.player.Play(serve(t, tt.body, tt.status), false); err != nil {
				t.Fatal(err)
			}
			if e := r.next(t); e.start || e.err == nil || !strings.Contains(e.err.Error(), tt.want) {
				t.Errorf("reported %+v, want an end naming %q", e, tt.want)
			}
		})
	}
}

func TestAClipInAnotherFormatIsRefused(t *testing.T) {
	b := fixture(t)
	packed := binary.BigEndian.Uint64(b[18:])
	packed = packed&^(0xfffff<<44) | 44100<<44
	binary.BigEndian.PutUint64(b[18:], packed)
	r := newRig(t)
	if err := r.player.Play(serve(t, b, http.StatusOK), false); err != nil {
		t.Fatal(err)
	}
	if e := r.next(t); e.start || !errors.Is(e.err, errFormat) {
		t.Errorf("a 44.1 kHz clip reported %+v, want errFormat: the listing asked for %d Hz,"+
			" and the feed is opened at that rate", e, Rate)
	}
}

func TestStoppingAClipStopsItsFeedAndReportsTheEndOnce(t *testing.T) {
	r := newRig(t)
	r.hold = make(chan struct{})
	if err := r.player.Play(serve(t, fixture(t), http.StatusOK), false); err != nil {
		t.Fatal(err)
	}
	if e := r.next(t); !e.start {
		t.Fatalf("reported %+v, want the start", e)
	}
	f := <-r.feeds
	r.player.Stop()
	close(r.hold)
	if e := r.next(t); e.start || e.err != nil {
		t.Fatalf("reported %+v, want a clean end", e)
	}
	r.quiet(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		t.Error("the feed was never stopped, so the mixer plays on")
	}
}

func TestANewClipReplacesTheOneThatIsPlaying(t *testing.T) {
	r := newRig(t)
	r.hold = make(chan struct{})
	url := serve(t, fixture(t), http.StatusOK)
	if err := r.player.Play(url, false); err != nil {
		t.Fatal(err)
	}
	if e := r.next(t); !e.start || e.announcement {
		t.Fatalf("reported %+v, want the media clip starting", e)
	}
	first := <-r.feeds
	if err := r.player.Play(url, true); err != nil {
		t.Fatal(err)
	}
	if e := r.next(t); e.start || e.announcement || e.err != nil {
		t.Fatalf("reported %+v, want the media clip ending at once: until then sendspin"+
			" stays unavailable through the new clip's fetch", e)
	}
	close(r.hold)
	if e := r.next(t); !e.start || !e.announcement {
		t.Fatalf("reported %+v, want the announcement starting", e)
	}
	if e := r.next(t); e.start || !e.announcement || e.err != nil {
		t.Fatalf("reported %+v, want the announcement ending", e)
	}
	r.quiet(t)
	first.mu.Lock()
	defer first.mu.Unlock()
	if !first.stopped {
		t.Error("the replaced clip's feed was never stopped")
	}
}

func TestAClipTheMixerStopsTakingEndsWithAReason(t *testing.T) {
	was := stallAfter
	stallAfter = 100 * time.Millisecond
	t.Cleanup(func() { stallAfter = was })
	r := newRig(t)
	r.stuck = true
	if err := r.player.Play(serve(t, fixture(t), http.StatusOK), false); err != nil {
		t.Fatal(err)
	}
	if e := r.next(t); !e.start {
		t.Fatalf("reported %+v, want the start", e)
	}
	if e := r.next(t); e.err == nil || !strings.Contains(e.err.Error(), "nothing moved") {
		t.Errorf("a clip fetched whole that the mixer stopped playing ended with %+v, want"+
			" the stall named: otherwise the player reads playing until somebody presses stop", e)
	}
}

func TestYieldStopsMediaButNotAnAnnouncement(t *testing.T) {
	for _, announcement := range []bool{false, true} {
		r := newRig(t)
		r.hold = make(chan struct{})
		if err := r.player.Play(serve(t, fixture(t), http.StatusOK), announcement); err != nil {
			t.Fatal(err)
		}
		r.next(t)
		r.player.Yield()
		close(r.hold)
		e := r.next(t)
		if announcement && e.err != nil {
			t.Errorf("an announcement ended with %v", e.err)
		}
		f := <-r.feeds
		f.mu.Lock()
		stopped := f.stopped
		f.mu.Unlock()
		if stopped == announcement {
			t.Errorf("announcement=%v: stopped=%v; alexa's music ends a media clip and"+
				" talks over an announcement", announcement, stopped)
		}
	}
}

func TestOnlyAPlausibleURLIsFetched(t *testing.T) {
	for _, url := range []string{"file:///etc/passwd", "ftp://x/y", "", "http://x/" + strings.Repeat("a", maxURL)} {
		if err := CheckURL(url); err == nil {
			t.Errorf("CheckURL(%.40q) passed", url)
		}
	}
	if err := CheckURL("http://192.168.53.5:8123/api/esphome/ffmpeg_proxy/a/b.flac"); err != nil {
		t.Errorf("the proxy's own url was refused: %v", err)
	}
}

func TestAStreamThatStallsEndsWithAReason(t *testing.T) {
	was := stallAfter
	stallAfter = 100 * time.Millisecond
	t.Cleanup(func() { stallAfter = was })
	body := fixture(t)[:4000]
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	r := newRig(t)
	if err := r.player.Play(srv.URL+"/x.flac", false); err != nil {
		t.Fatal(err)
	}
	for {
		e := r.next(t)
		if e.start {
			continue
		}
		if e.err == nil || !strings.Contains(e.err.Error(), "nothing moved") {
			t.Errorf("a stream that stopped sending ended with %v, want the stall named:"+
				" otherwise the player reads playing, and sendspin stays off, until somebody"+
				" presses stop", e.err)
		}
		return
	}
}

func TestAFetchThatFailsNamesTheCause(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/api/esphome/ffmpeg_proxy/" + strings.Repeat("a", 80) + ".flac"
	srv.Close()
	r := newRig(t)
	if err := r.player.Play(url, true); err != nil {
		t.Fatal(err)
	}
	e := r.next(t)
	if e.err == nil || strings.Contains(e.err.Error(), "ffmpeg_proxy") ||
		!strings.Contains(e.err.Error(), "refused") {
		t.Errorf("a refused fetch ended with %v, want the cause without the url: the log"+
			" cuts at 64 bytes, and the proxy's url alone is longer", e.err)
	}
}

func TestAStopWaitsForAStartBeingReported(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	p := &Player{
		Client: &http.Client{},
		Open: func(_ int, done func()) (Feed, error) {
			return &fakeFeed{done: done, hold: make(chan struct{})}, nil
		},
		OnStart: func(bool) {
			note("start")
			close(entered)
			<-release
		},
		OnEnd: func(bool, error) { note("end") },
	}
	if err := p.Play(serve(t, fixture(t), http.StatusOK), false); err != nil {
		t.Fatal(err)
	}
	<-entered
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop reported the end while the start was still being reported: a start" +
			" checked before the stop and reported after it leaves the player playing")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"start", "end"}) {
		t.Errorf("reported %v, want [start end]", order)
	}
}
