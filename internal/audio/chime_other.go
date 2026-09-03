//go:build !android || !cgo

package audio

import "errors"

// The player is OpenSL ES, which exists on the Dot and nowhere the tests run.
// This keeps the tree building and vetting for linux/arm under qemu, the way
// CLAUDE.md describes, with the audio path verified on the device instead.
//
// The tag covers !cgo as well as !android, so a cgo-less android build says
// what is missing rather than failing with an undefined NewChime.

type Chime struct{}

func NewChime() (*Chime, error) {
	return nil, errors.New("audio: built without OpenSL ES, which needs GOOS=android")
}

func (c *Chime) Play() error {
	return errors.New("audio: built without OpenSL ES")
}

func (c *Chime) Close() {}
