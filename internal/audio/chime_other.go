//go:build !android || !cgo

package audio

import "errors"

type Chime struct{}

func NewChime() (*Chime, error) {
	return nil, errors.New("audio: built without OpenSL ES, which needs GOOS=android")
}

func (c *Chime) Play() error {
	return errors.New("audio: built without OpenSL ES")
}

func (c *Chime) OpenStream(int, func(string, ...any)) (*Stream, error) {
	return nil, errors.New("audio: built without OpenSL ES")
}

func (c *Chime) Close() {}
