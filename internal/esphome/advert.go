package esphome

import (
	"strings"

	"github.com/bboe/overdub/internal/mdns"
)

const Service = "_esphomelib._tcp.local."

func Advert(instance, mac, board string, port uint16) mdns.Advert {
	return mdns.Advert{
		Service: Service,
		Port:    port,
		Records: []string{
			"mac=" + strings.ToLower(strings.ReplaceAll(mac, ":", "")),
			"api_encryption=" + noiseCipherName,
			"version=" + esphomeVersion,
			"friendly_name=" + instance,
			"platform=overdub",
			"board=" + board,
			"network=wifi",
		},
	}
}
