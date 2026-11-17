package avs

import (
	"bytes"
	"encoding/json"
	"html"
	"mime"
	"regexp"
	"strings"
)

const (
	maxHead = 4 << 10
	maxJSON = 64 << 10

	MaxReply = 255

	saidQueue = 16
)

const (
	seeking = iota
	inHead
	inJSON
	skipping
)

type watcher struct {
	delim  []byte
	buf    []byte
	state  int
	onPart func([]byte)
}

func boundaryOf(contentType string) string {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(media, "multipart/") {
		return ""
	}
	return params["boundary"]
}

func newWatcher(boundary string, onPart func([]byte)) *watcher {
	return &watcher{delim: []byte("\r\n--" + boundary), buf: []byte("\r\n"), onPart: onPart}
}

func (w *watcher) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for w.step() {
	}
	return len(p), nil
}

func (w *watcher) step() bool {
	switch w.state {
	case seeking, skipping:
		i := bytes.Index(w.buf, w.delim)
		if i < 0 {
			if keep := len(w.delim) - 1; len(w.buf) > keep {
				w.buf = append(w.buf[:0], w.buf[len(w.buf)-keep:]...)
			}
			return false
		}
		w.buf = w.buf[i+len(w.delim):]
		w.state = inHead
		return true

	case inHead:
		if len(w.buf) >= 2 && bytes.HasPrefix(w.buf, []byte("--")) {
			w.state = seeking
			return true
		}
		head, rest, found := bytes.Cut(w.buf, []byte("\r\n\r\n"))
		if !found {
			if len(w.buf) > maxHead {
				w.state = seeking
				return true
			}
			return false
		}
		w.state = skipping
		for _, line := range strings.Split(string(head), "\r\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.EqualFold(strings.TrimSpace(k), "Content-Type") &&
				strings.Contains(strings.ToLower(v), "json") {
				w.state = inJSON
			}
		}
		w.buf = rest
		return true

	default:
		i := bytes.Index(w.buf, w.delim)
		if i < 0 {
			if len(w.buf) > maxJSON {
				w.state = skipping
				return true
			}
			return false
		}
		w.onPart(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+len(w.delim):]
		w.state = inHead
		return true
	}
}

func spoken(part []byte) (string, bool) {
	var msg struct {
		Directive struct {
			Header struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"header"`
			Payload struct {
				Caption json.RawMessage `json:"caption"`
				SSML    string          `json:"ssml"`
			} `json:"payload"`
		} `json:"directive"`
	}
	if json.Unmarshal(part, &msg) != nil {
		return "", false
	}
	d := msg.Directive
	if d.Header.Namespace != "SpeechSynthesizer" || d.Header.Name != "Speak" {
		return "", false
	}
	said := captionText(d.Payload.Caption)
	if said == "" {
		said = ssmlText(d.Payload.SSML)
	}
	return said, said != ""
}

func captionText(raw json.RawMessage) string {
	var caption struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &caption) != nil || caption.Content == "" {
		var plain string
		if json.Unmarshal(raw, &plain) != nil {
			return ""
		}
		caption.Content = plain
	}
	lines := strings.Split(caption.Content, "\n")
	var cues []string
	for i, line := range lines {
		line = strings.TrimSpace(line)
		timing := i+1 < len(lines) && strings.Contains(lines[i+1], "-->")
		if line == "" || line == "WEBVTT" || strings.Contains(line, "-->") || timing {
			continue
		}
		cues = append(cues, line)
	}
	return squeeze(strings.Join(cues, " "))
}

var tag = regexp.MustCompile(`<[^>]*>`)

func ssmlText(ssml string) string {
	return squeeze(html.UnescapeString(tag.ReplaceAllString(ssml, " ")))
}

func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

func pieces(s string) []string {
	var out []string
	for len(s) > MaxReply {
		cut := strings.LastIndexByte(s[:MaxReply+1], ' ')
		if cut <= 0 {
			cut = MaxReply
			for cut > 0 && !utf8Start(s[cut]) {
				cut--
			}
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], " ")
	}
	return append(out, s)
}

func utf8Start(b byte) bool { return b&0xc0 != 0x80 }
