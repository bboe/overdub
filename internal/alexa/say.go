// Package alexa speaks through Alexa's own synthesizer.
// docs/architecture.md has the measurements.
package alexa

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed chime.mp3
var chime []byte

const (
	chimeHost    = "127.0.0.1"
	amPath       = "/system/bin/am"
	speakTimeout = 15 * time.Second
)

func ServeChime() (string, <-chan error, error) {
	listener, err := net.Listen("tcp", chimeHost+":0")
	if err != nil {
		return "", nil, fmt.Errorf("chime listener on %s: %w", chimeHost, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/chime.mp3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		http.ServeContent(w, r, "chime.mp3", time.Time{}, bytes.NewReader(chime))
	})
	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	return "http://" + listener.Addr().String() + "/chime.mp3", stopped, nil
}

var directiveSeq uint64

func Speak(url string) error {
	if err := checkSpeakURL(url); err != nil {
		return err
	}
	id := strconv.Itoa(os.Getpid())
	seq := strconv.FormatUint(atomic.AddUint64(&directiveSeq, 1), 10)
	ctx, cancel := context.WithTimeout(context.Background(), speakTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, amPath, speakArgs(url, id, seq)...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("am startservice: no answer in %v: %s",
				speakTimeout, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("am startservice: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if said := amError(string(out)); said != "" {
		return fmt.Errorf("am startservice: %s", said)
	}
	return nil
}

func speakArgs(url, id, seq string) []string {
	payload := fmt.Sprintf(`{"url":"%s"}`, url)
	return []string{"startservice",
		"-a", "com.amazon.speech.SpeechSynthesizer_Speak",
		"-n", "amazon.speech.sim/amazon.speech.agent.speechsynthesizer.SpeechSynthesizerAgent",
		"--es", "directiveId", id,
		"--es", "sequenceId", seq,
		"--es", "namespace", "SpeechSynthesizer",
		"--es", "name", "Speak",
		"--es", "payloadVersion", "1",
		"--es", "payload", payload,
		"--esa", "namespaces", "SpeechSynthesizer",
		"--esa", "names", "Speak",
		"--esa", "payloadVersions", "1",
		"--esa", "payloads", payload,
	}
}

func amError(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "Error:") {
			return line
		}
	}
	return ""
}

func checkSpeakURL(url string) error {
	if !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("url must be http://, which is the only scheme verified on the device: %q", url)
	}
	if strings.ContainsAny(url, `",`) {
		return fmt.Errorf("url must contain no comma or double quote, which the intent cannot carry: %q", url)
	}
	return nil
}
