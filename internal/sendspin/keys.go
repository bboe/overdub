package sendspin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	pskLen = 32
	keyLen = 32

	pskIDLabel    = "sendspin-psk-id-v1"
	sentinelLabel = "sendspin-sentinel-psk-v1"
)

type Identity struct {
	Private []byte
	Public  []byte
}

func (i Identity) ClientID() string { return EncodeID(i.Public) }

func EncodeID(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

func DecodeID(id string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return nil, fmt.Errorf("id is not base64url: %w", err)
	}
	if len(raw) != keyLen {
		return nil, fmt.Errorf("id decodes to %d bytes, want %d", len(raw), keyLen)
	}
	return raw, nil
}

func PSKID(psk []byte) string {
	sum := sha256.Sum256(append([]byte(pskIDLabel), psk...))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func SentinelPSK() []byte {
	sum := sha256.Sum256([]byte(sentinelLabel))
	return sum[:]
}

func NewPSK() ([]byte, error) {
	psk := make([]byte, pskLen)
	if _, err := rand.Read(psk); err != nil {
		return nil, fmt.Errorf("drawing a pre-shared key: %w", err)
	}
	return psk, nil
}
