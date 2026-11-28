//go:build !android || !cgo

package audio

import "errors"

type Chime struct{}

func NewChime() (*Chime, error) {
	return nil, errors.New("audio: built without the mixer, which needs GOOS=android")
}

func (c *Chime) Play() error {
	return errors.New("audio: built without the mixer")
}

func (c *Chime) PlayError() error {
	return errors.New("audio: built without the mixer")
}

func (c *Chime) OpenStream(int, func(string, ...any)) (*Stream, error) {
	return nil, errors.New("audio: built without the mixer")
}

func (c *Chime) OpenFeed(int, func()) (*Feed, error) {
	return nil, errors.New("audio: built without the mixer")
}

func (c *Chime) Streaming() bool { return false }

func (c *Chime) Duck(int) {}

func (c *Chime) DuckClips(int) {}

func (c *Chime) Close() {}
