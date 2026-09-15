package sendspin

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	msgJSON     byte = 0
	msgFragment byte = 1

	maxNoiseMessage = 65535
	aeadTagLen      = 16
	maxFrameBody    = maxNoiseMessage - aeadTagLen - 1

	fragLast  = 0x01
	fragFirst = 0x02
)

var errTransport = errors.New("sendspin transport error")

func (s *Session) WriteJSON(kind string, payload any) error {
	body, err := marshalEnvelope(kind, payload)
	if err != nil {
		return err
	}
	return s.WriteTyped(msgJSON, body)
}

func (s *Session) WriteTyped(kind byte, body []byte) error {
	if kind == msgFragment {
		return fmt.Errorf("%w: %#x is the fragment type", errTransport, kind)
	}
	s.writing.Lock()
	defer s.writing.Unlock()
	if len(body) <= maxFrameBody {
		return s.seal(append([]byte{kind}, body...))
	}
	first := true
	for len(body) > 0 {
		room := maxFrameBody - 1
		if first {
			room--
		}
		n := min(room, len(body))
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
		frame = append(frame, body[:n]...)
		if err := s.seal(frame); err != nil {
			return err
		}
		body = body[n:]
		first = false
	}
	return nil
}

func (s *Session) seal(plain []byte) error {
	sealed, err := s.send.Encrypt(nil, nil, plain)
	if err != nil {
		return fmt.Errorf("%w: %w", errTransport, err)
	}
	return s.ws.WriteBinary(sealed)
}

func (s *Session) Read() (kind byte, body []byte, err error) {
	frames := 0
	for {
		if frames++; frames > maxMessageFrames {
			return 0, nil, fmt.Errorf("%w: message split across more than %d frames",
				errTransport, maxMessageFrames)
		}
		isBinary, sealed, err := s.ws.Read()
		if err != nil {
			return 0, nil, err
		}
		if !isBinary {
			return 0, nil, fmt.Errorf("%w: cleartext frame after transport mode", errTransport)
		}
		plain, err := s.recv.Decrypt(nil, nil, sealed)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: %w", errTransport, err)
		}
		if len(plain) < 1 {
			return 0, nil, fmt.Errorf("%w: empty plaintext", errTransport)
		}
		if plain[0] != msgFragment {
			if s.inFragment {
				return 0, nil, fmt.Errorf(
					"%w: %#x arrived while a fragmented message was in flight", errTransport, plain[0])
			}
			return plain[0], plain[1:], nil
		}
		done, err := s.reassemble(plain)
		if err != nil {
			return 0, nil, err
		}
		if !done {
			continue
		}
		kind, body = s.fragmentType, s.fragment
		s.inFragment, s.fragment, s.fragmentType = false, nil, 0
		return kind, body, nil
	}
}

func (s *Session) reassemble(plain []byte) (bool, error) {
	if len(plain) < 2 {
		return false, fmt.Errorf("%w: fragment carries no flags", errTransport)
	}
	flags := plain[1]
	if flags&^byte(fragFirst|fragLast) != 0 {
		return false, fmt.Errorf("%w: fragment reserved flag bits set", errTransport)
	}
	data := plain[2:]
	if flags&fragFirst != 0 {
		if s.inFragment {
			return false, fmt.Errorf("%w: first fragment while one is in flight", errTransport)
		}
		if len(data) < 1 {
			return false, fmt.Errorf("%w: first fragment carries no type", errTransport)
		}
		if data[0] == msgFragment {
			return false, fmt.Errorf("%w: fragment names itself as its type", errTransport)
		}
		s.inFragment, s.fragmentType, s.fragment = true, data[0], append([]byte{}, data[1:]...)
	} else {
		if !s.inFragment {
			return false, fmt.Errorf("%w: continuation fragment with none in flight", errTransport)
		}
		s.fragment = append(s.fragment, data...)
	}
	if len(s.fragment) > maxMessage {
		return false, fmt.Errorf("%w: reassembled message past %d bytes", errTooBig, maxMessage)
	}
	return flags&fragLast != 0, nil
}

func (s *Session) ReadEnvelope() (string, json.RawMessage, error) {
	kind, body, err := s.Read()
	if err != nil {
		return "", nil, err
	}
	if kind != msgJSON {
		return "", nil, fmt.Errorf("%w: wanted a JSON message, got type %#x", errTransport, kind)
	}
	return decodeEnvelope(body)
}

func decodeEnvelope(body []byte) (string, json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", nil, fmt.Errorf("%w: %w", errTransport, err)
	}
	return env.Type, env.Payload, nil
}
