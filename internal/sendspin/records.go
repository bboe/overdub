package sendspin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const (
	recordLen      = keyLen + pskLen
	recordCapacity = maxConns + 1
)

type record struct {
	server []byte
	psk    []byte
}

func (r record) serverID() string { return EncodeID(r.server) }

type Records struct {
	path string

	mu   sync.Mutex
	list []record
}

func LoadRecords(path string) (*Records, error) {
	r := &Records{path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"%s is %#o, so a uid that is not root can read the long-term psks; want %#o",
			path, perm, keyFileMode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw)%recordLen != 0 {
		return nil, fmt.Errorf("%s is %d bytes, not a whole number of %d-byte records",
			path, len(raw), recordLen)
	}
	for at := 0; at < len(raw); at += recordLen {
		server, psk := raw[at:at+keyLen], raw[at+keyLen:at+recordLen]
		if allZero(server) || allZero(psk) {
			return nil, fmt.Errorf("%s: record %d is all zeros in one half", path, at/recordLen)
		}
		r.list = append(r.list, record{server: bytes.Clone(server), psk: bytes.Clone(psk)})
	}
	return r, nil
}

func (r *Records) find(id string) (record, bool) {
	if r == nil {
		return record{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.list {
		if PSKID(rec.psk) == id {
			return rec, true
		}
	}
	return record{}, false
}

func (r *Records) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.list)
}

var errNoStore = errors.New("this client keeps no pairing records")

func (r *Records) put(server, psk []byte, protected map[string]bool) error {
	if r == nil {
		return errNoStore
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var next []record
	for _, rec := range r.list {
		if bytes.Equal(rec.server, server) && !protected[PSKID(rec.psk)] {
			continue
		}
		next = append(next, rec)
	}
	next = append(next, record{server: bytes.Clone(server), psk: bytes.Clone(psk)})
	for i := 0; len(next) > recordCapacity && i < len(next)-1; {
		if protected[PSKID(next[i].psk)] {
			i++
			continue
		}
		next = slices.Delete(next, i, i+1)
	}
	return r.save(next)
}

func (r *Records) remove(id string) error {
	if r == nil {
		return errNoStore
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var next []record
	for _, rec := range r.list {
		if PSKID(rec.psk) != id {
			next = append(next, rec)
		}
	}
	if len(next) == len(r.list) {
		return nil
	}
	return r.save(next)
}

func (r *Records) save(next []record) error {
	raw := make([]byte, 0, len(next)*recordLen)
	for _, rec := range next {
		raw = append(append(raw, rec.server...), rec.psk...)
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), filepath.Base(r.path)+".new-")
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
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.list = next
	return nil
}
