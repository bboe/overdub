// Package evdev is the raw evdev and uinput plumbing, dependency-free so it
// cross-compiles to ARMv7 with CGO_ENABLED=0.
package evdev

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

const (
	evSyn = 0x00
	EvKey = 0x01

	synReport = 0x00

	// EV_KEY values. 2 is autorepeat, which the read loop ignores.
	KeyRelease = 0
	KeyPress   = 1

	// _IOW('E', 0x90, int)
	eviocgrab = 0x40044590

	// KEY_MAX is 0x2ff, so the key bitmap is 96 bytes.
	keyBytes = 96

	// EVIOCGBIT(EvKey, 96) = _IOC(_IOC_READ, 'E', 0x20+EvKey, 96)
	//   (2<<30) | (96<<16) | ('E'<<8) | 0x21
	eviocgbitKey = 0x80604521

	uinputNode = "/dev/uinput"

	uiSetEvbit   = 0x40045564 // _IOW(UINPUT_IOCTL_BASE, 100, int)
	uiSetKeybit  = 0x40045565 // _IOW(UINPUT_IOCTL_BASE, 101, int)
	uiDevCreate  = 0x5501     // _IO(UINPUT_IOCTL_BASE, 1)
	uiDevDestroy = 0x5502     // _IO(UINPUT_IOCTL_BASE, 2)

	EventSize = 16 // sec(4) + usec(4) + type(2) + code(2) + value(4)
)

// syscall.Timeval is 8 bytes only on a 32-bit target: a wrong-arch build
// is a compile error, not a daemon that misreads every event.
const _ = uint(8 - unsafe.Sizeof(syscall.Timeval{}))

type Event struct {
	Sec   int32
	Usec  int32
	Type  uint16
	Code  uint16
	Value int32
}

func (e Event) marshal() []byte {
	b := make([]byte, EventSize)
	binary.LittleEndian.PutUint32(b[0:], uint32(e.Sec))
	binary.LittleEndian.PutUint32(b[4:], uint32(e.Usec))
	binary.LittleEndian.PutUint16(b[8:], e.Type)
	binary.LittleEndian.PutUint16(b[10:], e.Code)
	binary.LittleEndian.PutUint32(b[12:], uint32(e.Value))
	return b
}

func Unmarshal(b []byte) Event {
	return Event{
		Sec:   int32(binary.LittleEndian.Uint32(b[0:])),
		Usec:  int32(binary.LittleEndian.Uint32(b[4:])),
		Type:  binary.LittleEndian.Uint16(b[8:]),
		Code:  binary.LittleEndian.Uint16(b[10:]),
		Value: int32(binary.LittleEndian.Uint32(b[12:])),
	}
}

func ioctl(fd uintptr, req uint, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func DeviceKeys(f *os.File) ([]uint16, error) {
	buf := make([]byte, keyBytes)
	if err := ioctl(f.Fd(), eviocgbitKey, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return nil, fmt.Errorf("EVIOCGBIT(EvKey): %w", err)
	}
	return keysFromBitmap(buf), nil
}

func keysFromBitmap(buf []byte) []uint16 {
	var keys []uint16
	for i := 0; i < len(buf)*8; i++ {
		if buf[i/8]&(1<<uint(i%8)) != 0 {
			keys = append(keys, uint16(i))
		}
	}
	return keys
}

func Grab(f *os.File, on bool) error {
	v := uintptr(0)
	if on {
		v = 1
	}
	if err := ioctl(f.Fd(), eviocgrab, v); err != nil {
		return fmt.Errorf("eviocgrab(%v): %w", on, err)
	}
	return nil
}

type Uinput struct{ f *os.File }

func NewUinput(name string, keys []uint16) (*Uinput, error) {
	file, err := os.OpenFile(uinputNode, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", uinputNode, err)
	}
	if err := ioctl(file.Fd(), uiSetEvbit, EvKey); err != nil {
		file.Close()
		return nil, fmt.Errorf("uiSetEvbit: %w", err)
	}
	for _, k := range keys {
		if err := ioctl(file.Fd(), uiSetKeybit, uintptr(k)); err != nil {
			file.Close()
			return nil, fmt.Errorf("uiSetKeybit(%d): %w", k, err)
		}
	}

	if _, err := file.Write(userDev(name)); err != nil {
		file.Close()
		return nil, fmt.Errorf("write uinput_user_dev: %w", err)
	}
	if err := ioctl(file.Fd(), uiDevCreate, 0); err != nil {
		file.Close()
		return nil, fmt.Errorf("uiDevCreate: %w", err)
	}
	return &Uinput{f: file}, nil
}

func userDev(name string) []byte {
	// struct uinput_user_dev, 32-bit layout:
	//   char name[80]; struct input_id{u16 x4}; int ff_effects_max;
	//   int absmax[64]; absmin[64]; absfuzz[64]; absflat[64]
	const devSize = 80 + 8 + 4 + 4*64*4
	buf := make([]byte, devSize)
	copy(buf[:79], name)                            // 79, so the name is always NUL-terminated
	binary.LittleEndian.PutUint16(buf[80:], 0x0003) // bustype BUS_USB
	binary.LittleEndian.PutUint16(buf[82:], 0x0001) // vendor
	binary.LittleEndian.PutUint16(buf[84:], 0x0001) // product
	binary.LittleEndian.PutUint16(buf[86:], 0x0001) // version
	return buf
}

func (u *Uinput) Emit(code uint16, value int32) error { return emit(u.f, code, value) }

func emit(w io.Writer, code uint16, value int32) error {
	if _, err := w.Write(Event{Type: EvKey, Code: code, Value: value}.marshal()); err != nil {
		return err
	}
	_, err := w.Write(Event{Type: evSyn, Code: synReport, Value: 0}.marshal())
	return err
}

func (u *Uinput) Close() error {
	_ = ioctl(u.f.Fd(), uiDevDestroy, 0)
	return u.f.Close()
}
