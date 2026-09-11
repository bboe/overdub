// Package alexa hands a clip to Alexa's own synthesizer, which is the only way
// to play one this daemon does not decode itself. docs/audio.md has the
// measurements and says what the route costs.
package alexa

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

const (
	speakTimeout = 15 * time.Second

	speakAction  = "com.amazon.speech.SpeechSynthesizer_Speak"
	speakService = "amazon.speech.sim/amazon.speech.agent.speechsynthesizer.SpeechSynthesizerAgent"
)

const Package = "amazon.speech.sim"

const installedTimeout = 10 * time.Second

var speakArgv = []string{"/system/bin/am", "startservice"}

var installedArgv = []string{"/system/bin/sh", "/system/bin/pm", "path", Package}

var installedCommand = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, installedArgv[0], installedArgv[1:]...).Output()
}

func Installed() bool {
	ctx, cancel := context.WithTimeout(context.Background(), installedTimeout)
	defer cancel()
	out, err := installedCommand(ctx)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "package:")
}

var speakCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, speakArgv[0], append(speakArgv[1:], args...)...)
	return cmd.CombinedOutput()
}

var directiveSeq atomic.Uint64

func Speak(url string) error {
	if err := CheckURL(url); err != nil {
		return err
	}
	n := directiveSeq.Add(1)
	args := speakArgs(url, fmt.Sprintf("dir%d-%d", os.Getpid(), n), fmt.Sprintf("seq%d-%d", os.Getpid(), n))

	ctx, cancel := context.WithTimeout(context.Background(), speakTimeout)
	defer cancel()
	out, err := speakCommand(ctx, args...)
	if err != nil {
		return fmt.Errorf("am startservice: %w: %s", err, cut(strings.TrimSpace(string(out))))
	}
	if said := amError(string(out)); said != "" {
		return fmt.Errorf("am startservice: %s", cut(said))
	}
	return nil
}

func speakArgs(url, id, seq string) []string {
	payload := fmt.Sprintf(`{"url":"%s"}`, url)
	return []string{
		"-a", speakAction,
		"-n", speakService,
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

const maxLoggedURL = 64

func CheckURL(url string) error {
	if !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("url must be http://, which is the only scheme verified on the device: %q",
			cut(url))
	}
	if strings.ContainsAny(url, "\",\\") {
		return fmt.Errorf("url must carry no comma, double quote or backslash: the extras are a "+
			"comma-separated array and the payload is hand-built json: %q", cut(url))
	}
	return nil
}

func cut(s string) string {
	if len(s) > maxLoggedURL {
		return s[:maxLoggedURL] + "..."
	}
	return s
}
