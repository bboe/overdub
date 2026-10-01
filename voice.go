package main

import (
	"errors"
	"log"
	"net"
	"net/url"
	"time"

	"github.com/bboe/overdub/internal/avs"
	"github.com/bboe/overdub/internal/device"
)

const (
	voiceIdentityPath = "/data/local/bin/.overdub-avs-identity"

	voicePort = 8443
	voiceAddr = "127.0.0.1:8443"
	voiceURL  = "https://" + voiceAddr
)

var voiceUpstream string

func startVoiceRelay() bool {
	cert, caPEM, err := avs.Identity(voiceIdentityPath)
	if err != nil {
		log.Printf("voice: no identity to serve: %v", err)
		return false
	}
	paths, err := avs.TrustPaths(caPEM)
	if err != nil {
		log.Printf("voice: %v", err)
		return false
	}
	body, err := avs.TrustFile(caPEM)
	if err != nil {
		log.Printf("voice: %v", err)
		return false
	}
	var missing []string
	for _, path := range paths {
		if !device.Trusted(path, body) {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		if err := device.Trust(missing, body); err != nil {
			log.Printf("voice: the device will not trust the relay, so Alexa is left alone: %v", err)
			return false
		}
		log.Printf("voice: trusted our own certificate authority as %v", missing)
	}
	upstream, err := device.SpeechUpstream(voiceURL)
	if err != nil {
		log.Printf("voice: no endpoint to pass Alexa's session on to: %v", err)
		return false
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Hostname() == "" {
		log.Printf("voice: %q is not an endpoint to pass Alexa's session on to", upstream)
		return false
	}
	target := u.Host
	if u.Port() == "" {
		target = net.JoinHostPort(u.Hostname(), "443")
	}
	relay := avs.New(voiceAddr, target, cert, device.Dialer(15*time.Second).DialContext)
	ln, err := relay.Listen()
	if err != nil {
		log.Printf("voice: %v", err)
		return false
	}
	if err := device.AllowLoopback(voicePort); err != nil {
		log.Printf("voice: Alexa could not reach the relay: %v", err)
		ln.Close()
		return false
	}
	held := make(chan struct{})
	go device.HoldLoopbackOpen(voicePort, firewallRe, held)
	go func() {
		if err := relay.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("voice: the relay stopped: %v", err)
		}
	}()
	voiceUpstream = upstream
	voiced.Store(true)
	if err := device.PointSpeech(voiceURL); err != nil {
		log.Printf("voice: Alexa was not pointed at the relay: %v", err)
		voiced.Store(false)
		stopVoiceRelay()
		ln.Close()
		close(held)
		return false
	}
	log.Printf("voice: Alexa's session goes through %s to %s", voiceAddr, upstream)
	return true
}

func relayVoice() {
	if startVoiceRelay() {
		return
	}
	if err := device.LeaveSpeech(voiceURL); err != nil {
		log.Printf("voice: Alexa may still be pointed at a relay that is not running: %v", err)
	}
}

func stopVoiceRelay() {
	if err := device.PointSpeech(voiceUpstream); err != nil {
		log.Printf("voice: Alexa is still pointed at a relay that is gone: %v", err)
		return
	}
	log.Printf("voice: Alexa's session goes to %s directly again", voiceUpstream)
}
