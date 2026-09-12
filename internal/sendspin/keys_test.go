package sendspin

import (
	"encoding/hex"
	"strings"
	"testing"
)

const specSentinelPSK = "1b5e24dbc1aed95fc2a5a338a90c05df44bd10f5ec1f4cd66cbf86272767b9d3"

const specSentinelPSKID = "185b15f6d2da4909bd1dc156a4ab206103abef0153bcd52d926170b95cf7ce8a"

const specSentinelPSKIDText = "GFsV9tLaSQm9HcFWpKsgYQOr7wFTvNUtkmFwuVz3zoo"

func TestSentinelPSKMatchesTheSpec(t *testing.T) {
	if got := hex.EncodeToString(SentinelPSK()); got != specSentinelPSK {
		t.Errorf("sentinel psk = %s\nwant            %s", got, specSentinelPSK)
	}
}

func TestSentinelPSKIDMatchesTheSpec(t *testing.T) {
	got := PSKID(SentinelPSK())
	if got != specSentinelPSKIDText {
		t.Errorf("sentinel psk_id = %s\nwant              %s", got, specSentinelPSKIDText)
	}
	raw, err := DecodeID(got)
	if err != nil {
		t.Fatalf("DecodeID: %v", err)
	}
	if h := hex.EncodeToString(raw); h != specSentinelPSKID {
		t.Errorf("sentinel psk_id = %s\nwant              %s", h, specSentinelPSKID)
	}
}

func TestPSKIDUsesTheURLAlphabet(t *testing.T) {
	// sha256("5") as a psk gives a psk_id carrying both - and _, which is what
	// separates base64url from standard base64. The sentinel vector carries
	// neither, so it is satisfied by either alphabet.
	psk, err := hex.DecodeString(
		"ef2d127de37b942baad06145e54b0c619a1f22327b2ebbcfbec78f5564afe39d")
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	const want = "-zraBRghtlU_WiGdO1hpDWL_x6ylHh3-WhFS5FM-FyA"
	if got := PSKID(psk); got != want {
		t.Errorf("psk_id = %s\nwant       %s", got, want)
	}
}

func TestDecodeIDRefusesTheWrongLength(t *testing.T) {
	for _, c := range []struct{ name, id string }{
		{"empty", ""},
		{"short", EncodeID(make([]byte, keyLen-1))},
		{"long", EncodeID(make([]byte, keyLen+1))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeID(c.id); err == nil {
				t.Errorf("DecodeID(%q) was accepted as a %d-byte key", c.id, keyLen)
			}
		})
	}
}

func TestTheClientIDIsUnpaddedBase64URL(t *testing.T) {
	// The id goes on the wire and into the log as the Dot's name for itself. Padding
	// it is caught elsewhere only as handshake timeouts, which say nothing about why.
	id := EncodeID(make([]byte, keyLen))
	if len(id) != 43 {
		t.Errorf("EncodeID gave %d characters, want 43 for 32 unpadded bytes: %q", len(id), id)
	}
	if strings.ContainsRune(id, '=') {
		t.Errorf("EncodeID padded the id: %q", id)
	}
}
