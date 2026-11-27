//go:build !android || !cgo

package opus

import "errors"

type Decoder struct{}

func NewDecoder() (*Decoder, error) {
	return nil, errors.New("opus: built without libopus, which needs GOOS=android")
}

func (d *Decoder) Decode([]byte) ([]int16, error) {
	return nil, errors.New("opus: built without libopus")
}

func (d *Decoder) Close() {}
