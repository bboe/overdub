// Command overdub takes over the Echo Dot's action button, and leaves the rest
// of Amazon's stack alone.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bboe/overdub/internal/alexa"
	"github.com/bboe/overdub/internal/evdev"
)

const (
	inputNode  = "/dev/input/event1"
	actionKey  = 138
	uinputName = "mtk-kpd"
)

var chiming int32

func main() {
	if err := intercept(); err != nil {
		fmt.Fprintln(os.Stderr, "overdub: "+err.Error())
		os.Exit(1)
	}
}

func intercept() error {
	if err := waitForNode(inputNode, 60*time.Second); err != nil {
		return err
	}

	chimeURL, chimeStopped, err := alexa.ServeChime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; presses will be silent\n", err)
	} else {
		go func() {
			fmt.Fprintf(os.Stderr, "overdub: chime server stopped: %v\n", <-chimeStopped)
			os.Exit(1)
		}()
	}

	file, err := os.Open(inputNode)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	keys, err := evdev.DeviceKeys(file)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("%s declares no keycodes", inputNode)
	}
	fmt.Printf("cloning %d keycodes from %s\n", len(keys), inputNode)
	uinput, err := evdev.NewUinput(uinputName, keys)
	if err != nil {
		return fmt.Errorf("uinput: %w (is CONFIG_UINPUT present, and are you root?)", err)
	}
	defer func() { _ = uinput.Close() }()

	if err := evdev.Grab(file, true); err != nil {
		return err
	}
	defer func() { _ = evdev.Grab(file, false) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = uinput.Close()
		_ = evdev.Grab(file, false)
		os.Exit(0)
	}()

	fmt.Printf("intercepting %s: consuming keycode %d, passing the rest to %q\n",
		inputNode, actionKey, uinputName)

	return route(file, uinput.Emit, actionKey, func(held time.Duration) {
		fmt.Printf("%s intercepted %d (held %v)\n", time.Now().Format("15:04:05.000"),
			actionKey, held.Round(time.Millisecond))
		if chimeURL != "" && atomic.CompareAndSwapInt32(&chiming, 0, 1) {
			go func() {
				defer atomic.StoreInt32(&chiming, 0)
				if err := alexa.Speak(chimeURL); err != nil {
					fmt.Fprintf(os.Stderr, "chime: %v\n", err)
				}
			}()
		}
	})
}

func route(r io.Reader, emit func(uint16, int32) error, consume uint16, onPress func(held time.Duration)) error {
	var pressedAt time.Time
	buf := make([]byte, evdev.EventSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		event := evdev.Unmarshal(buf)

		if event.Type == evdev.EvKey && event.Code == consume {
			switch event.Value {
			case 1:
				pressedAt = time.Now()
			case 0:
				if pressedAt.IsZero() {
					continue
				}
				onPress(time.Since(pressedAt))
				pressedAt = time.Time{}
			}
			continue
		}

		if event.Type == evdev.EvKey {
			if err := emit(event.Code, event.Value); err != nil {
				return fmt.Errorf("passthrough failed for %d: %w", event.Code, err)
			}
		}
	}
}

func waitForNode(path string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%s never appeared", path)
		}
		time.Sleep(min(remaining, time.Second))
	}
}
