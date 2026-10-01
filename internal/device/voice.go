package device

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	puffinService = "com.amazon.puffin"
	endpointProp  = "AVSEndpoint"

	mountTimeout = 15 * time.Second

	propPoll = 100 * time.Millisecond

	endpointLIPCTimeout = 10 * time.Second
)

var (
	puffinWaitFor = 2 * time.Minute
	puffinPoll    = time.Second
	propWaitFor   = 15 * time.Second
	sdkConfigPath = "/system/etc/AlexaClientSDKConfig.json"
	readEndpoint  = func() (string, error) {
		out, err := lipcReadWithin("-s", puffinService, endpointProp, endpointLIPCTimeout)
		return strings.TrimRight(out, "\r\n"), err
	}
	writeEndpoint = func(url string) error {
		return lipcSetString(puffinService, endpointProp, url, endpointLIPCTimeout)
	}
	sdkComment = regexp.MustCompile(`(?m)^\s*//.*$`)
)

func awaitEndpoint() (string, error) {
	deadline := time.Now().Add(puffinWaitFor)
	for {
		now, err := readEndpoint()
		if err == nil || time.Now().After(deadline) {
			return now, err
		}
		time.Sleep(puffinPoll)
	}
}

func SpeechUpstream(ours string) (string, error) {
	now, err := awaitEndpoint()
	if err != nil {
		return "", err
	}
	if now != ours && strings.HasPrefix(now, "https://") {
		return now, nil
	}
	return StockSpeechEndpoint()
}

func LeaveSpeech(ours string) error {
	now, err := awaitEndpoint()
	if err != nil {
		return err
	}
	if now != ours {
		return nil
	}
	stock, err := StockSpeechEndpoint()
	if err != nil {
		return err
	}
	return PointSpeech(stock)
}

func StockSpeechEndpoint() (string, error) {
	raw, err := os.ReadFile(sdkConfigPath)
	if err != nil {
		return "", err
	}
	var config struct {
		ACL struct {
			Endpoint string `json:"endpoint"`
		} `json:"acl"`
	}
	if err := json.Unmarshal(sdkComment.ReplaceAll(raw, nil), &config); err != nil {
		return "", fmt.Errorf("reading %s: %w", sdkConfigPath, err)
	}
	if !strings.HasPrefix(config.ACL.Endpoint, "https://") {
		return "", fmt.Errorf("%s names no https acl endpoint", sdkConfigPath)
	}
	return config.ACL.Endpoint, nil
}

func PointSpeech(url string) error {
	if now, err := readEndpoint(); err == nil && now == url {
		return nil
	}
	if err := writeEndpoint(url); err != nil {
		return err
	}
	var got string
	var err error
	for deadline := time.Now().Add(propWaitFor); ; {
		if got, err = readEndpoint(); err == nil && got == url {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(propPoll)
	}
	if err != nil {
		return fmt.Errorf("reading %s back: %w", endpointProp, err)
	}
	return fmt.Errorf("%s read back as %q, not %q", endpointProp, got, url)
}

func Trusted(path string, pem []byte) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		return false
	}
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, pem)
}

func Trust(paths []string, pem []byte) error {
	if err := remount("rw"); err != nil {
		return err
	}
	defer func() {
		if err := remount("ro"); err != nil {
			fmt.Fprintf(os.Stderr, "leaving / writable: %v\n", err)
		}
	}()
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, pem, 0o644); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o644); err != nil {
			return err
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, pem) {
			return fmt.Errorf("%s did not read back as it was written", path)
		}
	}
	return nil
}

var remount = func(how string) error {
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/system/bin/mount", "-o", how+",remount", "/")
	cmd.WaitDelay = 500 * time.Millisecond
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remounting / %s: %w: %s", how, err, bytes.TrimSpace(out))
	}
	return nil
}
