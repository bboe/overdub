package esphome

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type pb struct{ b []byte }

const (
	wireVarint  = 0
	wireFixed32 = 5
	wireBytes   = 2
)

func (p *pb) uvarint(v uint64) {
	for v >= 0x80 {
		p.b = append(p.b, byte(v)|0x80)
		v >>= 7
	}
	p.b = append(p.b, byte(v))
}

func (p *pb) tag(field, wire int) { p.uvarint(uint64(field)<<3 | uint64(wire)) }

func (p *pb) u32(field int, v uint32) { p.tag(field, wireVarint); p.uvarint(uint64(v)) }

func (p *pb) boolean(field int, v bool) {
	p.tag(field, wireVarint)
	if v {
		p.uvarint(1)
	} else {
		p.uvarint(0)
	}
}

func (p *pb) str(field int, s string) {
	p.tag(field, wireBytes)
	p.uvarint(uint64(len(s)))
	p.b = append(p.b, s...)
}

func (p *pb) fixed32(field int, v uint32) {
	p.tag(field, wireFixed32)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	p.b = append(p.b, buf[:]...)
}

func (p *pb) float(field int, f float32) { p.fixed32(field, math.Float32bits(f)) }

type pbField struct {
	field int
	wire  int
	num   uint64
	data  []byte
}

func pbWalk(msg []byte, visit func(pbField)) error {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return fmt.Errorf("bad field tag")
		}
		msg = msg[n:]
		if key>>3 > 0x1fffffff {
			return fmt.Errorf("field number out of range")
		}
		entry := pbField{field: int(key >> 3), wire: int(key & 7)}
		switch entry.wire {
		case wireVarint:
			v, n := binary.Uvarint(msg)
			if n <= 0 {
				return fmt.Errorf("bad varint in field %d", entry.field)
			}
			entry.num, msg = v, msg[n:]
		case wireFixed32:
			if len(msg) < 4 {
				return fmt.Errorf("short fixed32 in field %d", entry.field)
			}
			entry.num, msg = uint64(binary.LittleEndian.Uint32(msg)), msg[4:]
		case wireBytes:
			l, n := binary.Uvarint(msg)
			if n <= 0 || uint64(len(msg)-n) < l {
				return fmt.Errorf("short bytes in field %d", entry.field)
			}
			entry.data, msg = msg[n:n+int(l)], msg[n+int(l):]
		case 1: // fixed64
			if len(msg) < 8 {
				return fmt.Errorf("short fixed64 in field %d", entry.field)
			}
			entry.num, msg = binary.LittleEndian.Uint64(msg), msg[8:]
		default:
			return fmt.Errorf("unsupported wire type %d in field %d", entry.wire, entry.field)
		}
		visit(entry)
	}
	return nil
}

func writeFrame(w io.Writer, msgType int, payload []byte) error {
	var h pb
	h.b = append(h.b, 0x00)
	h.uvarint(uint64(len(payload)))
	h.uvarint(uint64(msgType))
	if _, err := w.Write(append(h.b, payload...)); err != nil {
		return err
	}
	return nil
}

// What real ESPHome accepts: api_frame_helper.h sets MAX_MESSAGE_SIZE to 32768
// on esp32 and refuses anything longer, so a client that talks to ESPHome
// devices has no reason to send more. The length itself is a varint and not a
// 16-bit field, so the framing does not bound this and the constant has to.
// What arrives here is a command or a subscription, hundreds of bytes, and this
// is also what an unauthenticated peer can make the daemon allocate on a device
// with 256 MB shared with Android and Alexa.
const maxFrame = 32768

func readFrame(reader *bufio.Reader) (int, []byte, error) {
	lead, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if lead != 0x00 {
		if lead == 0x01 {
			return 0, nil, fmt.Errorf("encrypted frame on a plaintext connection; the stream is out of step")
		}
		return 0, nil, fmt.Errorf("bad frame lead byte 0x%02x", lead)
	}
	length, err := binary.ReadUvarint(reader)
	if err != nil {
		return 0, nil, err
	}
	if length > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the %d byte limit", length, maxFrame)
	}
	msgType, err := binary.ReadUvarint(reader)
	if err != nil {
		return 0, nil, err
	}
	var payload bytes.Buffer
	if _, err := io.CopyN(&payload, reader, int64(length)); err != nil {
		return 0, nil, err
	}
	return int(msgType), payload.Bytes(), nil
}
