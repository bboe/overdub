package esphome

import (
	"bytes"
	"math"
	"testing"
)

func TestUvarintKnownEncodings(t *testing.T) {
	tests := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{math.MaxUint32, []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
	}
	for _, tt := range tests {
		var p pb
		p.uvarint(tt.v)
		if !bytes.Equal(p.b, tt.want) {
			t.Errorf("uvarint(%d) = % x, want % x", tt.v, p.b, tt.want)
		}
	}
}

func TestTagPacksFieldAndWireType(t *testing.T) {
	var p pb
	p.tag(1, wireVarint)
	p.tag(2, wireBytes)
	p.tag(16, wireFixed32)
	want := []byte{0x08, 0x12, 0x85, 0x01}
	if !bytes.Equal(p.b, want) {
		t.Errorf("tags = % x, want % x", p.b, want)
	}
}

func TestFixed32IsLittleEndianAndFourBytes(t *testing.T) {
	var p pb
	p.fixed32(1, 0x01020304)
	want := []byte{0x0d, 0x04, 0x03, 0x02, 0x01}
	if !bytes.Equal(p.b, want) {
		t.Errorf("fixed32 = % x, want % x", p.b, want)
	}
}

func TestStrAndBooleanAndFloat(t *testing.T) {
	var p pb
	p.str(1, "hi")
	p.boolean(2, true)
	p.boolean(3, false)
	p.float(4, 1.5)

	var got []pbField
	if err := pbWalk(p.b, func(f pbField) { got = append(got, f) }); err != nil {
		t.Fatalf("pbWalk: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("decoded %d fields, want 4", len(got))
	}
	if string(got[0].data) != "hi" {
		t.Errorf("field 1 = %q, want %q", got[0].data, "hi")
	}
	if got[1].num != 1 || got[2].num != 0 {
		t.Errorf("booleans = %d, %d, want 1, 0", got[1].num, got[2].num)
	}
	if f := math.Float32frombits(uint32(got[3].num)); f != 1.5 {
		t.Errorf("float = %v, want 1.5", f)
	}
}

func TestWalkRoundTrip(t *testing.T) {
	var p pb
	p.u32(1, 300)
	p.str(2, "name")
	p.fixed32(3, 0xdeadbeef)

	seen := map[int]pbField{}
	if err := pbWalk(p.b, func(f pbField) { seen[f.field] = f }); err != nil {
		t.Fatalf("pbWalk: %v", err)
	}
	if seen[1].num != 300 {
		t.Errorf("field 1 = %d, want 300", seen[1].num)
	}
	if string(seen[2].data) != "name" {
		t.Errorf("field 2 = %q, want name", seen[2].data)
	}
	if seen[3].num != 0xdeadbeef {
		t.Errorf("field 3 = %#x, want 0xdeadbeef", seen[3].num)
	}
}

func TestWalkRejectsMalformed(t *testing.T) {
	var full pb
	full.str(1, "abcdefgh")
	for n := 1; n < len(full.b); n++ {
		if err := pbWalk(full.b[:n], func(pbField) {}); err == nil {
			t.Errorf("pbWalk accepted a %d-byte prefix of a %d-byte message", n, len(full.b))
		}
	}
	if err := pbWalk([]byte{0x0f}, func(pbField) {}); err == nil {
		t.Error("pbWalk accepted wire type 7")
	}
}

func TestWalkSkipsFixed64(t *testing.T) {
	msg := []byte{0x09, 1, 2, 3, 4, 5, 6, 7, 8} // field 1, wire type 1
	msg = append(msg, 0x10, 0x2a)               // field 2, varint 42
	var last pbField
	if err := pbWalk(msg, func(f pbField) { last = f }); err != nil {
		t.Fatalf("pbWalk: %v", err)
	}
	if last.field != 2 || last.num != 42 {
		t.Errorf("after fixed64 got field %d = %d, want field 2 = 42", last.field, last.num)
	}
}

func TestWalkReportsAMalformedMessage(t *testing.T) {
	if err := walk("test", []byte{0x08}, func(pbField) {}); err == nil {
		t.Error("a varint running past the end was reported as understood")
	}
	if err := walk("test", []byte{0x08, 0x01}, func(pbField) {}); err != nil {
		t.Errorf("a well-formed message was reported as malformed: %v", err)
	}
}
