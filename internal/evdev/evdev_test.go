package evdev

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestEventLayoutIs32Bit(t *testing.T) {
	if got := unsafe.Sizeof(syscall.Timeval{}); got != 8 {
		t.Fatalf("timeval is %d bytes, want 8: this is not a 32-bit userspace", got)
	}
	if EventSize != 16 {
		t.Errorf("EventSize = %d, want 16", EventSize)
	}
	if got := len(Event{}.marshal()); got != EventSize {
		t.Errorf("marshal produced %d bytes, want %d", got, EventSize)
	}
}

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	sizeofInt = 4

	keyMax = 0x2ff
)

func ioc(dir, size uint, typ byte, nr uint) uint {
	return dir<<30 | size<<16 | uint(typ)<<8 | nr
}

func TestIoctlRequestsMatchTheKernelMacros(t *testing.T) {
	for _, tt := range []struct {
		macro string
		have  uint
		want  uint
	}{
		{"_IOW('E', 0x90, int)", eviocgrab, ioc(iocWrite, sizeofInt, 'E', 0x90)},
		{"_IOC(_IOC_READ, 'E', 0x20+EV_KEY, keyBytes)", eviocgbitKey,
			ioc(iocRead, keyBytes, 'E', 0x20+EvKey)},
		{"_IOR('E', 0x02, struct input_id)", eviocgid, ioc(iocRead, idBytes, 'E', 0x02)},
		{"_IOW('U', 100, int)", uiSetEvbit, ioc(iocWrite, sizeofInt, 'U', 100)},
		{"_IOW('U', 101, int)", uiSetKeybit, ioc(iocWrite, sizeofInt, 'U', 101)},
		{"_IO('U', 1)", uiDevCreate, ioc(iocNone, 0, 'U', 1)},
		{"_IO('U', 2)", uiDevDestroy, ioc(iocNone, 0, 'U', 2)},
	} {
		if tt.have != tt.want {
			t.Errorf("%s is 0x%08x, the constant is 0x%08x", tt.macro, tt.want, tt.have)
		}
	}
}

func TestKeyBitmapHoldsEveryKeycode(t *testing.T) {
	if want := (keyMax + 8) / 8; keyBytes != want {
		t.Errorf("keyBytes = %d, want %d to hold keycodes 0 to KEY_MAX (0x%x)",
			keyBytes, want, keyMax)
	}
}

func TestEventRoundTrip(t *testing.T) {
	for _, e := range []Event{
		{},
		{Sec: 1, Usec: 2, Type: EvKey, Code: 138, Value: 1},
		{Sec: -1, Usec: -1, Type: 0xffff, Code: 0xffff, Value: -1},
		{Sec: 1 << 30, Usec: 999999, Type: evSyn, Code: synReport, Value: 0},
	} {
		if got := Unmarshal(e.marshal()); got != e {
			t.Errorf("round trip of %+v gave %+v", e, got)
		}
	}
}

func TestUnmarshalKnownBytes(t *testing.T) {
	b := []byte{
		0x01, 0x00, 0x00, 0x00, // sec = 1
		0x40, 0x42, 0x0f, 0x00, // usec = 1000000
		0x01, 0x00, // type = EvKey
		0x8a, 0x00, // code = 138, the action button
		0xff, 0xff, 0xff, 0xff, // value = -1
	}
	want := Event{Sec: 1, Usec: 1000000, Type: EvKey, Code: 138, Value: -1}
	if got := Unmarshal(b); got != want {
		t.Errorf("Unmarshal = %+v, want %+v", got, want)
	}
}

func TestKeysFromBitmap(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want []uint16
	}{
		{"empty", make([]byte, 4), nil},
		{"bit 0 is keycode 0", []byte{0x01}, []uint16{0}},
		{"lsb first, not msb", []byte{0x02}, []uint16{1}},
		{"high bit of first byte", []byte{0x80}, []uint16{7}},
		{"first bit of second byte", []byte{0x00, 0x01}, []uint16{8}},
		{"all of one byte", []byte{0xff}, []uint16{0, 1, 2, 3, 4, 5, 6, 7}},
	}
	for _, tt := range tests {
		if got := keysFromBitmap(tt.buf); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: keysFromBitmap(%#v) = %v, want %v", tt.name, tt.buf, got, tt.want)
		}
	}
}

func TestKeysFromBitmapFindsActionAndMute(t *testing.T) {
	buf := make([]byte, keyBytes)
	for _, code := range []uint16{113, 138} {
		buf[code/8] |= 1 << (code % 8)
	}
	want := []uint16{113, 138}
	if got := keysFromBitmap(buf); !reflect.DeepEqual(got, want) {
		t.Errorf("keysFromBitmap = %v, want %v", got, want)
	}
}

func TestEmitFollowsEveryKeyWithASyncReport(t *testing.T) {
	var buf bytes.Buffer
	if err := emit(&buf, 113, 1); err != nil {
		t.Fatal(err)
	}
	if got := buf.Len(); got != 2*EventSize {
		t.Fatalf("emit wrote %d bytes, want two events (%d)", got, 2*EventSize)
	}
	key := Unmarshal(buf.Bytes()[:EventSize])
	if want := (Event{Type: EvKey, Code: 113, Value: 1}); key != want {
		t.Errorf("first event is %+v, want %+v", key, want)
	}
	sync := Unmarshal(buf.Bytes()[EventSize:])
	if want := (Event{Type: evSyn, Code: synReport, Value: 0}); sync != want {
		t.Errorf("second event is %+v, want a SYN_REPORT %+v", sync, want)
	}
}

type shortWriter struct {
	n   int
	err error
}

func (w *shortWriter) Write(b []byte) (int, error) {
	if w.n == 0 {
		return 0, w.err
	}
	w.n--
	return len(b), nil
}

func TestEmitReportsAFailedWrite(t *testing.T) {
	boom := errors.New("no such device")
	for _, ok := range []int{0, 1} {
		if err := emit(&shortWriter{n: ok, err: boom}, 115, 0); !errors.Is(err, boom) {
			t.Errorf("with %d writes accepted, emit returned %v, want %v", ok, err, boom)
		}
	}
}

func TestUserDevMatchesTheKernelStruct(t *testing.T) {
	const (
		uinputMaxNameSize = 80
		inputIDSize       = 4 * 2 // bustype, vendor, product, version
		ffEffectsMaxSize  = 4
		absCnt            = 64 // ABS_MAX + 1
		absArrays         = 4  // absmax, absmin, absfuzz, absflat
	)
	want := uinputMaxNameSize + inputIDSize + ffEffectsMaxSize + absArrays*absCnt*4
	if got := len(userDev("mtk-kpd", InputID{})); got != want {
		t.Fatalf("uinput_user_dev is %d bytes, want %d", got, want)
	}
}

// Each field lands where the kernel struct says, and each carries what the real
// node reported rather than a constant. Android reads all four: the bus decides
// IsExternal, and vendor and product are what a keylayout is looked up by
// before the name is tried. Biscuit's own values, so a field written to the
// wrong offset is visible as a number that belongs somewhere else.
func TestUserDevPlacesTheNameAndTheIDItWasGiven(t *testing.T) {
	id := InputID{Bus: 0x0019, Vendor: 0x2454, Product: 0x6500, Version: 0x0010}
	buf := userDev("mtk-kpd", id)
	if got := string(buf[:7]); got != "mtk-kpd" {
		t.Errorf("name is %q, want %q at offset 0", got, "mtk-kpd")
	}
	if buf[7] != 0 {
		t.Error("the name is not NUL-terminated")
	}
	for _, tt := range []struct {
		field string
		at    int
		want  uint16
	}{
		{"bustype", 80, 0x0019}, // BUS_HOST, which is what makes it internal
		{"vendor", 82, 0x2454},
		{"product", 84, 0x6500},
		{"version", 86, 0x0010},
	} {
		if got := binary.LittleEndian.Uint16(buf[tt.at:]); got != tt.want {
			t.Errorf("%s at offset %d is 0x%04x, want 0x%04x", tt.field, tt.at, got, tt.want)
		}
	}
	if !bytes.Equal(buf[88:], make([]byte, len(buf)-88)) {
		t.Error("ff_effects_max and the abs arrays are not zero")
	}
}

// Each field is read from its own offset, in the kernel's order. Biscuit's own
// values, so a field read from the wrong one shows up as a number belonging to
// another field rather than as a plausible one.
func TestIDFromBytesReadsTheKernelOrder(t *testing.T) {
	buf := []byte{0x19, 0x00, 0x54, 0x24, 0x00, 0x65, 0x10, 0x00}
	want := InputID{Bus: 0x0019, Vendor: 0x2454, Product: 0x6500, Version: 0x0010}
	if got := idFromBytes(buf); got != want {
		t.Errorf("idFromBytes = %+v, want %+v", got, want)
	}
}

// The decode and the encode are the same four fields in the same order, so a
// change to one and not the other lands here rather than on the device, where
// it would surface as Android resolving a different keylayout and nothing
// saying so.
func TestTheIDSurvivesTheRoundTrip(t *testing.T) {
	want := InputID{Bus: 0x0019, Vendor: 0x2454, Product: 0x6500, Version: 0x0010}
	if got := idFromBytes(userDev("mtk-kpd", want)[80:88]); got != want {
		t.Errorf("the id came back as %+v, want %+v", got, want)
	}
}

func TestUserDevTruncatesALongName(t *testing.T) {
	buf := userDev(strings.Repeat("x", 200), InputID{Bus: 0x0019})
	if got := bytes.IndexByte(buf[:80], 0); got != 79 {
		t.Errorf("a 200-byte name leaves the first NUL at %d, want 79", got)
	}
	if got := binary.LittleEndian.Uint16(buf[80:]); got != 0x0019 {
		t.Errorf("a long name overwrote bustype: 0x%04x", got)
	}
}
