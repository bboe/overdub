package sendspin

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const keyFileLen = keyLen + pskLen

const keyFileMode fs.FileMode = 0o600

var errZeroKey = errors.New(
	"key file is all zeros in one half, so it is not a key: delete it and let the daemon make another")

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

type Keys struct {
	Identity   Identity
	PairingPSK []byte
}

func loadKeys(path string) (Keys, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Keys{}, err
	}
	if !info.Mode().IsRegular() {
		return Keys{}, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Keys{}, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Keys{}, fmt.Errorf(
			"%s is %#o, so a uid that is not root can read the pairing psk; want %#o",
			path, perm, keyFileMode)
	}
	keys, err := decodeKeys(raw)
	if err != nil {
		return Keys{}, fmt.Errorf("%s: %w", path, err)
	}
	return keys, nil
}

func LoadOrCreateKeys(path string) (Keys, bool, error) {
	switch _, err := os.Lstat(path); {
	case err == nil:
		keys, err := loadKeys(path)
		return keys, false, err
	case !os.IsNotExist(err):
		return Keys{}, false, err
	}

	priv, err := NewPSK()
	if err != nil {
		return Keys{}, false, err
	}
	psk, err := NewPSK()
	if err != nil {
		return Keys{}, false, err
	}
	raw := append(append([]byte{}, priv...), psk...)
	if err := installKeyFile(path, raw); err != nil {
		if os.IsExist(err) {
			keys, lerr := loadKeys(path)
			return keys, false, lerr
		}
		return Keys{}, false, err
	}
	keys, err := decodeKeys(raw)
	if err != nil {
		return Keys{}, false, err
	}
	return keys, true, nil
}

func installKeyFile(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".new-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Link(tmp, path)
}

func decodeKeys(raw []byte) (Keys, error) {
	if len(raw) != keyFileLen {
		return Keys{}, fmt.Errorf("key file is %d bytes, want %d", len(raw), keyFileLen)
	}
	if allZero(raw[:keyLen]) || allZero(raw[keyLen:]) {
		return Keys{}, errZeroKey
	}
	id, err := identityFromPrivate(raw[:keyLen])
	if err != nil {
		return Keys{}, err
	}
	return Keys{Identity: id, PairingPSK: bytes.Clone(raw[keyLen:])}, nil
}

func identityFromPrivate(priv []byte) (Identity, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return Identity{}, fmt.Errorf("static private key is not a valid X25519 scalar: %w", err)
	}
	return Identity{Private: bytes.Clone(priv), Public: k.PublicKey().Bytes()}, nil
}
