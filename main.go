// Command overdub takes over the Echo Dot's action button, and leaves the rest
// of Amazon's stack alone.
package main

import (
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bboe/overdub/internal/alexa"
	"github.com/bboe/overdub/internal/button"
)

const (
	inputNode  = "/dev/input/event1"
	nodeWait   = 60 * time.Second
	actionKey  = 138
	uinputName = "mtk-kpd"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)
	if err := intercept(); err != nil {
		log.Printf("overdub: %v", err)
		os.Exit(1)
	}
}

func intercept() error {
	i, err := button.Open(inputNode, actionKey, uinputName, nodeWait)
	if err != nil {
		return err
	}
	defer i.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		i.Close()
		os.Exit(0)
	}()

	chimeURL, chimeStopped, err := alexa.ServeChime()
	if err != nil {
		log.Printf("warning: %v; presses will be silent", err)
	} else {
		go func() {
			log.Printf("chime server stopped: %v", <-chimeStopped)
			os.Exit(1)
		}()
	}

	log.Printf("intercepting %s: consuming keycode %d, passing the rest to %q",
		inputNode, actionKey, uinputName)

	var chiming atomic.Bool
	return i.Run(func(held time.Duration) {
		log.Printf("intercepted %d (held %v)", actionKey, held.Round(time.Millisecond))
		if chimeURL == "" || !chiming.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer chiming.Store(false)
			if err := alexa.Speak(chimeURL); err != nil {
				log.Printf("chime: %v", err)
			}
		}()
	})
}
