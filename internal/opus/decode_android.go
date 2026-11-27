package opus

/*
#cgo LDFLAGS: -ldl
#include "opus.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const maxFrame = Rate * 120 / 1000

type Decoder struct {
	mu sync.Mutex
	st unsafe.Pointer
}

func NewDecoder() (*Decoder, error) {
	st := C.overdub_opus_new(Rate, 1)
	if st == nil {
		return nil, errors.New("opus: libopus would not load or make a decoder")
	}
	return &Decoder{st: st}, nil
}

func (d *Decoder) Decode(packet []byte) ([]int16, error) {
	if len(packet) == 0 {
		return nil, errors.New("opus: an empty packet")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st == nil {
		return nil, errors.New("opus: decode after close")
	}
	pcm := make([]int16, maxFrame)
	n := C.overdub_opus_decode(d.st, (*C.uchar)(unsafe.Pointer(&packet[0])), C.int(len(packet)),
		(*C.short)(unsafe.Pointer(&pcm[0])), C.int(maxFrame))
	if n < 0 {
		return nil, fmt.Errorf("opus: decode failed: %d", int(n))
	}
	return append([]int16(nil), pcm[:n]...), nil
}

func (d *Decoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	C.overdub_opus_free(d.st)
	d.st = nil
}
