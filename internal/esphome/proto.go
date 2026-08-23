package esphome

import (
	"encoding/binary"
	"fmt"
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
