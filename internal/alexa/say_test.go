package alexa

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestCheckSpeakURL(t *testing.T) {
	if err := checkSpeakURL("http://192.0.2.10:8123/local/dot-tts/0-1.mp3"); err != nil {
		t.Errorf("a plain http URL was rejected: %v", err)
	}

	bad := []struct {
		name string
		url  string
	}{
		{"file scheme", "file:///data/local/tmp/a.mp3"},
		{"https is unverified", "https://example.invalid/a.mp3"},
		{"no scheme", "example.invalid/a.mp3"},
		{"comma", "http://example.invalid/a,b.mp3"},
		{"double quote", `http://example.invalid/a".mp3`},
	}
	for _, tt := range bad {
		if err := checkSpeakURL(tt.url); err == nil {
			t.Errorf("%s: checkSpeakURL(%q) returned no error", tt.name, tt.url)
		}
	}
}

func TestChimeIsTheOneEncodingAlexaAccepts(t *testing.T) {
	if len(chime) < 4 {
		t.Fatal("chime.mp3 is missing or truncated")
	}
	frames := 0
	for at := 0; at < len(chime); frames++ {
		if at+4 > len(chime) {
			t.Fatalf("frame %d at offset %d: %d bytes left, want a 4-byte header",
				frames, at, len(chime)-at)
		}
		h := chime[at : at+4]
		if h[0] != 0xff || h[1]&0xe0 != 0xe0 {
			t.Fatalf("frame %d at offset %d: no frame sync: % x", frames, at, h)
		}
		for _, f := range []struct {
			what string
			got  byte
			want byte
		}{
			{"MPEG version", h[1] >> 3 & 3, 2},      // MPEG-2, which 24 kHz implies
			{"layer", h[1] >> 1 & 3, 1},             // Layer III
			{"bitrate index", h[2] >> 4 & 0xf, 6},   // 48 kbps in every frame, so CBR
			{"sample rate index", h[2] >> 2 & 3, 1}, // 24 kHz
			{"channel mode", h[3] >> 6 & 3, 3},      // mono
		} {
			if f.got != f.want {
				t.Fatalf("frame %d at offset %d: %s = %d, want %d",
					frames, at, f.what, f.got, f.want)
			}
		}
		at += 72*48000/24000 + int(h[2]>>1&1)
		if at > len(chime) {
			t.Fatalf("frame %d runs %d bytes past the end", frames, at-len(chime))
		}
	}
	if frames < 2 {
		t.Errorf("chime.mp3 holds %d frame(s), too short to be the measured clip", frames)
	}
	if head := string(chime[:72*48000/24000]); strings.Contains(head, "Xing") ||
		strings.Contains(head, "Info") {
		t.Error("frame 0 is a Xing/LAME info frame; re-encode with -write_xing 0")
	}
}

func TestServeChimeIsReachableOnlyFromLoopback(t *testing.T) {
	served, stopped, err := ServeChime()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(served)
	if err != nil {
		t.Fatalf("ServeChime returned %q: %v", served, err)
	}
	if u.Hostname() != chimeHost {
		t.Errorf("chime is served from %q, want %q: any other bind is reachable "+
			"from the LAN", u.Hostname(), chimeHost)
	}

	resp, err := http.Get(served)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %s, want 200", served, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type is %q, want audio/mpeg", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, chime) {
		t.Errorf("served %d bytes, want the %d embedded ones", len(body), len(chime))
	}

	other, err := http.Get("http://" + u.Host + "/other.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Body.Close() }()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown path returned %s, want 404", other.Status)
	}

	select {
	case err := <-stopped:
		t.Errorf("the chime server stopped on its own: %v", err)
	default:
	}
}

func TestSpeakArgsCarryTheMeasuredIntent(t *testing.T) {
	const served = "http://127.0.0.1:44217/chime.mp3"
	payload := `{"url":"` + served + `"}`
	want := []string{"startservice",
		"-a", "com.amazon.speech.SpeechSynthesizer_Speak",
		"-n", "amazon.speech.sim/amazon.speech.agent.speechsynthesizer.SpeechSynthesizerAgent",
		"--es", "directiveId", "352",
		"--es", "sequenceId", "7",
		"--es", "namespace", "SpeechSynthesizer",
		"--es", "name", "Speak",
		"--es", "payloadVersion", "1",
		"--es", "payload", payload,
		"--esa", "namespaces", "SpeechSynthesizer",
		"--esa", "names", "Speak",
		"--esa", "payloadVersions", "1",
		"--esa", "payloads", payload,
	}
	if got := speakArgs(served, "352", "7"); !reflect.DeepEqual(got, want) {
		t.Errorf("speakArgs =\n%q\nwant\n%q", got, want)
	}
}

func TestAmErrorFindsAFailureAfterTheAnnouncement(t *testing.T) {
	const out = "Starting service: Intent { act=com.amazon.speech.SpeechSynthesizer_Speak }\n" +
		"Error: Not found; no service started.\n"
	if got := amError(out); got != "Error: Not found; no service started." {
		t.Errorf("amError = %q, want the Error: line", got)
	}
}

func TestAmErrorIgnoresASuccessfulStart(t *testing.T) {
	if got := amError("Starting service: Intent { act=com.amazon.speech.SpeechSynthesizer_Speak }\n"); got != "" {
		t.Errorf("amError = %q, want none", got)
	}
}
