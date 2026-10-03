package avs

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// proxied stands a relay in front of upstream and returns a client that reaches
// it the way the speech app does: over TLS, speaking h2, asking for Amazon.
func proxied(t *testing.T, upstream http.Handler) (*http.Client, *Relay) {
	t.Helper()
	amazon := httptest.NewUnstartedServer(upstream)
	amazon.EnableHTTP2 = true
	amazon.StartTLS()
	t.Cleanup(amazon.Close)

	cert, _, err := Identity(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatalf("minting an identity: %v", err)
	}
	r := New("127.0.0.1:0", cert)
	r.target = strings.TrimPrefix(amazon.URL, "https://")
	r.tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	ln, err := r.Listen()
	if err != nil {
		t.Fatalf("listening for the relay: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go r.Serve(ln)

	client := &http.Client{Transport: &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialTLSContext: func(ctx context.Context, network, _ string, cfg *tls.Config) (net.Conn, error) {
			d := &tls.Dialer{Config: cfg}
			return d.DialContext(ctx, network, ln.Addr().String())
		},
	}}
	return client, r
}

func TestTheProxyForwardsARequestAndItsAnswerUnchanged(t *testing.T) {
	var got *http.Request
	var body []byte
	client, _ := proxied(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = req
		body, _ = io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "multipart/related; boundary=abc")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("answered"))
	}))

	req, err := http.NewRequest(http.MethodPost, "https://"+Endpoint+"/v20160207/events?x=1",
		strings.NewReader("an utterance"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the proxy answered nothing: %v", err)
	}
	defer resp.Body.Close()

	if got.Method != http.MethodPost {
		t.Errorf("Amazon saw method %q, want %q", got.Method, http.MethodPost)
	}
	if want := "/v20160207/events?x=1"; got.URL.RequestURI() != want {
		t.Errorf("Amazon saw %q, want %q: a path or query dropped here is a request she rejects",
			got.URL.RequestURI(), want)
	}
	if want := "Bearer token"; got.Header.Get("Authorization") != want {
		t.Errorf("Amazon saw Authorization %q, want %q: without it she answers nothing",
			got.Header.Get("Authorization"), want)
	}
	if string(body) != "an utterance" {
		t.Errorf("Amazon saw the body %q, want %q", body, "an utterance")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("the device saw status %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	if want := "multipart/related; boundary=abc"; resp.Header.Get("Content-Type") != want {
		t.Errorf("the device saw Content-Type %q, want %q: it is how she finds the parts",
			resp.Header.Get("Content-Type"), want)
	}
	back, _ := io.ReadAll(resp.Body)
	if string(back) != "answered" {
		t.Errorf("the device saw the body %q, want %q", back, "answered")
	}
}

// Amazon's downchannel is one response held open for the session. A directive
// buffered here is a directive the device never acts on, and the response's own
// headers are buffered with it, so the deadline is what reports that.
func TestTheProxyPassesADirectiveOnBeforeTheResponseEnds(t *testing.T) {
	release := make(chan struct{})
	client, _ := proxied(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("second"))
	}))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://"+Endpoint+"/v20160207/directives", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("no answer arrived while the response was still open, so every directive "+
			"waits for the session to end: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("reading the first directive: %v", err)
	}
	if got := string(buf); got != "first" {
		t.Errorf("the device read %q before the response ended, want %q", got, "first")
	}
}

// Amazon answers the downchannel with a status and a content type, and then
// nothing at all until a directive arrives. An http2 ResponseWriter holds the
// headers until the first write, so without a flush of its own the device waits
// for headers on a stream that was already answered.
func TestHeadersReachTheDeviceBeforeAnyBody(t *testing.T) {
	block := make(chan struct{})
	client, _ := proxied(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "multipart/related; boundary=abc")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-block
	}))
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+Endpoint+"/v20160207/directives", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the device never saw the downchannel's headers: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got == "" {
		t.Error("no Content-Type reached the device")
	}
}

// Amazon unreachable is not a status Amazon sent. The device is told what it
// would be told by a connection that broke, which its own retry handles.
func TestAnUnreachableAmazonBreaksTheStream(t *testing.T) {
	client, relay := proxied(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	relay.target = "127.0.0.1:1" // nothing listens there

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://"+Endpoint+"/v20160207/directives", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("the device was answered %d, want a broken stream", resp.StatusCode)
	}
}

// flakyListener fails its first Accept with an error that is not a closed
// listener, the way a descriptor shortage does.
type flakyListener struct {
	net.Listener
	failed bool
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("accept: too many open files")
	}
	return f.Listener.Accept()
}

// The speech app reconnects by itself, so one failed accept must not leave the
// device with nowhere to go: the properties still point here.
func TestOneFailedAcceptDoesNotEndTheRelay(t *testing.T) {
	amazon := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("answered")) }))
	amazon.EnableHTTP2 = true
	amazon.StartTLS()
	t.Cleanup(amazon.Close)

	cert, _, err := Identity(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	r := New("127.0.0.1:0", cert)
	r.target = strings.TrimPrefix(amazon.URL, "https://")
	r.tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	ln, err := r.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	flaky := &flakyListener{Listener: ln}
	go r.Serve(flaky)

	client := &http.Client{Transport: &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialTLSContext: func(ctx context.Context, network, _ string, cfg *tls.Config) (net.Conn, error) {
			d := &tls.Dialer{Config: cfg}
			return d.DialContext(ctx, network, ln.Addr().String())
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+Endpoint+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the relay stopped accepting after one failure: %v", err)
	}
	defer resp.Body.Close()
	if got, _ := io.ReadAll(resp.Body); string(got) != "answered" {
		t.Errorf("the device read %q, want %q", got, "answered")
	}
}
