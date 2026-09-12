package esphome

import (
	"strings"
	"testing"
)

func TestTXTFriendlyNameMatchesTheName(t *testing.T) {
	var got string
	for _, e := range Advert("kitchen", "00:00:5E:00:53:2A", 6053).Records {
		if strings.HasPrefix(e, "friendly_name=") {
			got = strings.TrimPrefix(e, "friendly_name=")
		}
	}
	if got != "kitchen" {
		t.Errorf("friendly_name = %q, want %q", got, "kitchen")
	}
}

func TestTXTCarriesTheMAC(t *testing.T) {
	var got string
	for _, e := range Advert("kitchen", "00:00:5E:00:53:2A", 6053).Records {
		if strings.HasPrefix(e, "mac=") {
			got = strings.TrimPrefix(e, "mac=")
		}
	}
	if got != "00005e00532a" {
		t.Errorf("mac = %q, want %q", got, "00005e00532a")
	}
}

func TestTXTAnnouncesTheEncryption(t *testing.T) {
	want := "api_encryption=" + noiseCipherName
	for _, entry := range Advert("kitchen", "00:00:5E:00:53:2A", 6053).Records {
		if entry == want {
			return
		}
	}
	t.Errorf("txt() = %q, want it to carry %q", Advert("kitchen", "00:00:5E:00:53:2A", 6053).Records, want)
}
