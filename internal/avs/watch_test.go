package avs

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const amazonBoundary = "------abcde123"

func speakPart(dialog, caption, ssml string) string {
	return `{"directive":{"header":{"namespace":"SpeechSynthesizer","name":"Speak",` +
		`"messageId":"m","dialogRequestId":"` + dialog + `"},"payload":{"caption":` + caption +
		`,"format":"AUDIO_MPEG","ssml":"` + ssml + `","url":"cid:a"}}}`
}

func reply(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("\r\n--" + amazonBoundary + "\r\n")
		if strings.HasPrefix(p, "{") {
			b.WriteString("Content-Type: application/json; charset=UTF-8\r\n\r\n")
		} else {
			b.WriteString("Content-ID: <a>\r\nContent-Type: application/octet-stream\r\n\r\n")
		}
		b.WriteString(p)
	}
	b.WriteString("\r\n--" + amazonBoundary + "--\r\n")
	return b.String()
}

func watch(t *testing.T, stream string, chunk int) []string {
	t.Helper()
	var heard []string
	w := newWatcher(amazonBoundary, func(part []byte) {
		if text, ok := spoken(part); ok {
			heard = append(heard, pieces(text)...)
		}
	})
	for b := []byte(stream); len(b) > 0; {
		n := min(chunk, len(b))
		w.Write(b[:n])
		b = b[n:]
	}
	return heard
}

const webvtt = `{"content":"WEBVTT\n\n1\n00:00.000 --> 00:01.200\nIt's 9:41\n\n2\n00:01.200 --> 00:02.000\nin the morning.","type":"WEBVTT"}`

func TestTheReplyIsTheCaptionWhateverTheChunking(t *testing.T) {
	stream := reply(
		`{"directive":{"header":{"namespace":"SpeechRecognizer","name":"StopCapture"},"payload":{}}}`,
		speakPart("d1", webvtt, "<speak>It's 9:41 in the morning.</speak>"),
		strings.Repeat("\xff\x00--", 50000))
	for _, chunk := range []int{1, 7, 16 << 10, len(stream)} {
		got := watch(t, stream, chunk)
		if len(got) != 1 || got[0] != "It's 9:41 in the morning." {
			t.Errorf("in chunks of %d, heard %q; want the caption's two cues as one line", chunk, got)
		}
	}
}

func TestAReplyWithNoCaptionIsReadFromItsSSML(t *testing.T) {
	stream := reply(speakPart("d1", `null`, `<speak><prosody rate=\"fast\">Tom &amp; Jerry</prosody> are here.</speak>`))
	if got := watch(t, stream, 5); len(got) != 1 || got[0] != "Tom & Jerry are here." {
		t.Errorf("heard %q, want the SSML's words", got)
	}
}

func TestEachSpeakIsItsOwnReply(t *testing.T) {
	stream := reply(
		speakPart("d1", `{"content":"Here is the weather."}`, ""),
		speakPart("d1", `{"content":"Expect rain."}`, ""),
		speakPart("d2", `{"content":"Timer done."}`, ""))
	got := watch(t, stream, 11)
	want := []string{"Here is the weather.", "Expect rain.", "Timer done."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("heard %q, want %q", got, want)
	}
}

func TestASpeakWithNothingToSayIsNotAReply(t *testing.T) {
	if got := watch(t, reply(speakPart("d1", `{"content":""}`, "<speak></speak>")), 3); len(got) != 0 {
		t.Errorf("heard %q from a Speak with no words", got)
	}
}

func TestALongReplyIsSplitIntoWhatASensorHolds(t *testing.T) {
	for _, long := range []string{
		strings.Repeat("é", 300),
		strings.TrimSpace(strings.Repeat("Tomorrow's mostly cloudy. ", 30)),
	} {
		got := watch(t, reply(speakPart("d1", `{"content":"`+long+`"}`, "")), 100)
		if len(got) < 2 {
			t.Fatalf("heard %d replies from %d bytes, want it split", len(got), len(long))
		}
		for _, piece := range got {
			if len(piece) > MaxReply || !utf8.ValidString(piece) || strings.TrimSpace(piece) != piece {
				t.Errorf("a piece of %d bytes, %q; want at most %d, cut on a character", len(piece),
					piece, MaxReply)
			}
		}
		sep := ""
		if strings.Contains(long, " ") {
			sep = " "
		}
		if joined := strings.Join(got, sep); joined != long {
			t.Errorf("the pieces join to %q; want every word of the answer", joined)
		}
	}
}

func TestANumberOnItsOwnCueLineIsPartOfTheReply(t *testing.T) {
	caption := `{"content":"WEBVTT\n\n1\n00:00.000 --> 00:01.000\nThe answer is\n42\n\n2\n00:01.000 --> 00:02.000\n2026"}`
	got := watch(t, reply(speakPart("d1", caption, "")), 9)
	if len(got) != 1 || got[0] != "The answer is 42 2026" {
		t.Errorf("heard %q; only the line before a timing line is a cue identifier", got)
	}
}

func TestABoundaryInsideAudioIsNotAPart(t *testing.T) {
	audio := "ID3--" + amazonBoundary + "\r\nContent-Type: application/json\r\n\r\n" +
		speakPart("d1", `{"content":"Not said."}`, "")
	got := watch(t, reply(speakPart("d1", `{"content":"Before."}`, ""), audio,
		speakPart("d1", `{"content":"After."}`, "")), 13)
	if strings.Join(got, "|") != "Before.|After." {
		t.Errorf("heard %q; a boundary not at a line start is audio, not a delimiter", got)
	}
}

func TestAWatcherHoldsNoMoreThanAPartsWorth(t *testing.T) {
	w := newWatcher(amazonBoundary, func([]byte) {})
	w.Write([]byte("--" + amazonBoundary + "\r\nContent-Type: application/json\r\n\r\n"))
	for range 100 {
		w.Write(bytes.Repeat([]byte("x"), 4<<10))
	}
	if len(w.buf) > maxJSON+(4<<10) {
		t.Errorf("a JSON part with no end kept %d bytes", len(w.buf))
	}
	w.Write(bytes.Repeat([]byte("y"), 1<<20))
	if len(w.buf) >= len(w.delim) {
		t.Errorf("skipping kept %d bytes, want less than one delimiter", len(w.buf))
	}
}

func TestTheRelayReportsAlexasReplyOnlyWhileWatching(t *testing.T) {
	stream := reply(speakPart("d1", `{"content":"It's 9:41"}`, ""), "mp3 bytes",
		speakPart("d1", `{"content":"in the morning."}`, ""), "more mp3")
	client, r := proxied(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary=`+amazonBoundary+
			`; start=metadata.1; type="application/json"`)
		w.Write([]byte(stream))
	}))
	var mu sync.Mutex
	var heard []string
	r.OnSpeech(func(text string) {
		mu.Lock()
		heard = append(heard, text)
		mu.Unlock()
	})
	fetch := func() string {
		t.Helper()
		resp, err := client.Get("https://" + Endpoint + "/v20160207/directives")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	if body := fetch(); body != stream {
		t.Fatalf("the reply changed on its way through")
	}
	r.Watch(true)
	if body := fetch(); body != stream {
		t.Fatalf("watching changed the reply on its way through")
	}
	time.Sleep(300 * time.Millisecond)
	r.Watch(false)
	fetch()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(heard, "|") != "It's 9:41|in the morning." {
		t.Errorf("heard %q; want each Speak of the fetch made while watching, in order", heard)
	}
}
