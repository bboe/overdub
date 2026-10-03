package main

import (
	"log"
	"net"

	"github.com/bboe/overdub/internal/avs"
	"github.com/bboe/overdub/internal/device"
)

const (
	voiceIdentityPath = "/data/local/bin/.overdub-avs-identity"

	voiceAddr = "127.0.0.1:8443"
)

// startVoiceRelay puts Alexa's session through this device. It reports whether
// anything has to be undone at shutdown.
func startVoiceRelay() bool {
	cert, caPEM, err := avs.Identity(voiceIdentityPath)
	if err != nil {
		log.Printf("voice: no identity to serve: %v", err)
		return false
	}
	trust, err := avs.TrustPath(caPEM)
	if err != nil {
		log.Printf("voice: %v", err)
		return false
	}
	if !device.Trusted(trust) {
		body, err := avs.TrustFile(caPEM)
		if err != nil {
			log.Printf("voice: %v", err)
			return false
		}
		if err := device.Trust(trust, body); err != nil {
			log.Printf("voice: the device will not trust the relay, so Alexa is left alone: %v", err)
			return false
		}
		log.Printf("voice: trusted our own certificate authority as %s", trust)
	}
	relay := avs.New(voiceAddr, cert)
	ln, err := relay.Listen()
	if err != nil {
		log.Printf("voice: %v", err)
		return false
	}
	host, port, err := net.SplitHostPort(voiceAddr)
	if err != nil {
		log.Printf("voice: %v", err)
		ln.Close()
		return false
	}
	moved, err := device.RedirectSpeech(host, port)
	if err != nil {
		log.Printf("voice: Alexa was not pointed at the relay: %v", err)
		ln.Close()
		return false
	}
	log.Printf("voice: Alexa's session goes through %s", voiceAddr)
	if moved {
		if err := device.RestartSpeech(); err != nil {
			log.Printf("voice: the speech app kept its old connection: %v", err)
		}
	}
	go func() {
		if err := relay.Serve(ln); err != nil {
			log.Printf("voice: the relay stopped: %v", err)
		}
	}()
	return true
}

func stopVoiceRelay() {
	if err := device.ReleaseSpeech(); err != nil {
		log.Printf("voice: Alexa is still pointed at a relay that is gone: %v", err)
		return
	}
	log.Printf("voice: Alexa's session goes to Amazon directly again")
}
