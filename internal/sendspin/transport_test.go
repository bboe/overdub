package sendspin

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fragmentFrames(t *testing.T, kind byte, body []byte, chunk int) [][]byte {
	t.Helper()
	var out [][]byte
	first := true
	for len(body) > 0 {
		n := min(chunk, len(body))
		flags := byte(0)
		if first {
			flags |= fragFirst
		}
		if n == len(body) {
			flags |= fragLast
		}
		frame := []byte{msgFragment, flags}
		if first {
			frame = append(frame, kind)
		}
		out = append(out, append(frame, body[:n]...))
		body = body[n:]
		first = false
	}
	return out
}

func TestReadReassemblesAFragmentedMessage(t *testing.T) {
	session, server, peer := pairedSession(t)
	body, err := marshalEnvelope("server/state", map[string]any{"note": "fragmented"})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	frames := fragmentFrames(t, msgJSON, body, 12)
	if len(frames) < 3 {
		t.Fatalf("wanted at least three fragments, got %d", len(frames))
	}
	go func() {
		for _, f := range frames {
			sealed, err := server.send.Encrypt(nil, nil, f)
			if err != nil {
				return
			}
			peer.writeBinary(sealed)
		}
	}()
	kind, payload, err := session.ReadEnvelope()
	if err != nil {
		t.Fatalf("ReadEnvelope: %v", err)
	}
	if kind != "server/state" {
		t.Errorf("type = %q", kind)
	}
	if !bytes.Contains(payload, []byte("fragmented")) {
		t.Errorf("payload = %s", payload)
	}
}

func TestWriteFragmentsABodyPastTheFrameLimit(t *testing.T) {
	session, server, peer := pairedSession(t)
	body := bytes.Repeat([]byte("abcdefgh"), maxFrameBody/4)
	if len(body) <= maxFrameBody {
		t.Fatalf("test body is %d bytes, not past the %d limit", len(body), maxFrameBody)
	}
	go func() { _ = session.WriteTyped(msgJSON, body) }()

	var got []byte
	var kind byte
	for n := 0; ; n++ {
		if n > 8 {
			t.Fatal("never saw a last fragment")
		}
		op, sealed := peer.read()
		if op != opBinary {
			t.Fatalf("opcode %#x", op)
		}
		plain, err := server.recv.Decrypt(nil, nil, sealed)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if plain[0] != msgFragment {
			t.Fatalf("frame %d is type %#x, want a fragment", n, plain[0])
		}
		flags, data := plain[1], plain[2:]
		if n == 0 {
			if flags&fragFirst == 0 {
				t.Error("first frame does not carry the first-fragment flag")
			}
			kind, data = data[0], data[1:]
		} else if flags&fragFirst != 0 {
			t.Errorf("frame %d carries the first-fragment flag", n)
		}
		got = append(got, data...)
		if flags&fragLast != 0 {
			break
		}
	}
	if kind != msgJSON {
		t.Errorf("reassembled type = %#x, want %#x", kind, msgJSON)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("reassembled %d bytes, sent %d", len(got), len(body))
	}
}

func TestWriteRefusesToSendTheFragmentType(t *testing.T) {
	session, _, _ := pairedSession(t)
	if err := session.WriteTyped(msgFragment, []byte("x")); !errors.Is(err, errTransport) {
		t.Errorf("err = %v, want %v", err, errTransport)
	}
}

func TestReassembleRefusesMalformedSequences(t *testing.T) {
	for _, c := range []struct {
		name   string
		frames [][]byte
	}{
		{"continuation with none in flight", [][]byte{
			{msgFragment, 0x00, 'a'},
		}},
		{"first fragment while one is in flight", [][]byte{
			{msgFragment, fragFirst, msgJSON, 'a'},
			{msgFragment, fragFirst, msgJSON, 'b'},
		}},
		{"reserved flag bits", [][]byte{
			{msgFragment, fragFirst | 0x40, msgJSON, 'a'},
		}},
		{"fragment names itself as its type", [][]byte{
			{msgFragment, fragFirst, msgFragment, 'a'},
		}},
		{"no flags", [][]byte{
			{msgFragment},
		}},
		{"first fragment with no type", [][]byte{
			{msgFragment, fragFirst},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Session{}
			var err error
			for _, f := range c.frames {
				if _, err = s.reassemble(f); err != nil {
					break
				}
			}
			if !errors.Is(err, errTransport) {
				t.Errorf("err = %v, want %v", err, errTransport)
			}
		})
	}
}

func TestReassembleRefusesAMessagePastTheCeiling(t *testing.T) {
	s := &Session{}
	if _, err := s.reassemble(append([]byte{msgFragment, fragFirst, msgJSON},
		bytes.Repeat([]byte("x"), 1000)...)); err != nil {
		t.Fatalf("first fragment: %v", err)
	}
	var err error
	for range (maxMessage / 1000) + 2 {
		if _, err = s.reassemble(append([]byte{msgFragment, 0x00},
			bytes.Repeat([]byte("y"), 1000)...)); err != nil {
			break
		}
	}
	if !errors.Is(err, errTooBig) {
		t.Errorf("err = %v, want %v", err, errTooBig)
	}
}

func TestReadRefusesADataFrameMidFragment(t *testing.T) {
	session, server, peer := pairedSession(t)
	go func() {
		for _, f := range [][]byte{
			{msgFragment, fragFirst, msgJSON, 'a'},
			append([]byte{msgJSON}, []byte(`{"type":"x","payload":{}}`)...),
		} {
			sealed, err := server.send.Encrypt(nil, nil, f)
			if err != nil {
				return
			}
			peer.writeBinary(sealed)
		}
	}()
	if _, _, err := session.Read(); !errors.Is(err, errTransport) {
		t.Errorf("err = %v, want %v", err, errTransport)
	}
}

func TestReadRefusesAnEmptyPlaintext(t *testing.T) {
	session, server, peer := pairedSession(t)
	go func() {
		sealed, err := server.send.Encrypt(nil, nil, nil)
		if err != nil {
			return
		}
		peer.writeBinary(sealed)
	}()
	if _, _, err := session.Read(); !errors.Is(err, errTransport) {
		t.Errorf("err = %v, want %v", err, errTransport)
	}
}

func TestReadEnvelopeRefusesANonJSONType(t *testing.T) {
	session, server, peer := pairedSession(t)
	go func() {
		sealed, err := server.send.Encrypt(nil, nil, append([]byte{4}, []byte(`{"type":"x"}`)...))
		if err != nil {
			return
		}
		peer.writeBinary(sealed)
	}()
	if _, _, err := session.ReadEnvelope(); !errors.Is(err, errTransport) {
		t.Errorf("err = %v, want %v", err, errTransport)
	}
}

func TestReadReturnsABinaryRoleMessage(t *testing.T) {
	session, server, peer := pairedSession(t)
	go func() {
		sealed, err := server.send.Encrypt(nil, nil, []byte{4, 'p', 'c', 'm'})
		if err != nil {
			return
		}
		peer.writeBinary(sealed)
	}()
	kind, body, err := session.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if kind != 4 {
		t.Errorf("type = %#x, want 4", kind)
	}
	if string(body) != "pcm" {
		t.Errorf("body = %q", body)
	}
}

func TestWriteJSONProducesTheEnvelopeTheServerExpects(t *testing.T) {
	session, server, peer := pairedSession(t)
	go func() { _ = session.WriteJSON("client/state", map[string]any{"available": true}) }()
	_, sealed := peer.read()
	kind, body := server.open(t, sealed)
	if kind != msgJSON {
		t.Fatalf("type = %#x", kind)
	}
	var env struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if env.Type != "client/state" {
		t.Errorf("type = %q", env.Type)
	}
	if string(env.Payload) != `{"available":true}` {
		t.Errorf("payload = %s", env.Payload)
	}
}

func TestTheFrameLayoutIsFixedAgainstLiterals(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"json message type", int(msgJSON), 0},
		{"fragment message type", int(msgFragment), 1},
		{"fragment last flag", int(fragLast), 0x01},
		{"fragment first flag", int(fragFirst), 0x02},
		{"noise message ceiling", maxNoiseMessage, 65535},
		{"aead tag", aeadTagLen, 16},
		{"frame body", maxFrameBody, 65518},
		{"frames per message", maxMessageFrames, 4096},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestAFragmentedMessageLeavesNoStateBehind(t *testing.T) {
	session, server, peer := pairedSession(t)
	body, err := marshalEnvelope("test/first", struct{ N int }{1})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	go func() {
		for _, f := range fragmentFrames(t, msgJSON, body, 40) {
			peer.writeBinary(server.seal(t, f))
		}
		peer.writeBinary(server.sealJSON(t, "test/second", struct{ N int }{2}))
	}()
	if _, _, err := session.Read(); err != nil {
		t.Fatalf("first message: %v", err)
	}
	kind, second, err := session.Read()
	if err != nil {
		t.Fatalf("a message after a fragmented one failed: %v", err)
	}
	if kind != msgJSON || !strings.Contains(string(second), "test/second") {
		t.Errorf("second message = %#x %q", kind, second)
	}
}
