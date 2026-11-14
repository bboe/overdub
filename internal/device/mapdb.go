package device

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

const (
	mapTable     = "deviceData"
	mapTokenKey  = "refresh_token"
	maxMapDB     = 16 << 20
	mapReadTries = 5
)

var (
	mapDBPath  = "/data/ace/kvstorage/map.db"
	mapReadGap = 50 * time.Millisecond

	journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}
)

func RegistrationReadBudget() time.Duration {
	return time.Duration(mapReadTries) * mapReadGap
}

func AlexaToken() (string, error) {
	value, found, err := mapValue(mapTokenKey)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s holds no %s: this dot is registered to no amazon account",
			mapDBPath, mapTokenKey)
	}
	return string(value), nil
}

func AlexaRegistered() (registered, ok bool) {
	_, found, err := mapValue(mapTokenKey)
	if err != nil {
		return false, false
	}
	return found, true
}

func mapValue(key string) ([]byte, bool, error) {
	db, err := readSettled(mapDBPath)
	if err != nil {
		return nil, false, err
	}
	return sqliteLookup(db, mapTable, key)
}

func readSettled(path string) ([]byte, error) {
	var last []byte
	hot := 0
	for try := 0; try < mapReadTries; try++ {
		if try > 0 {
			time.Sleep(mapReadGap)
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if st.Size() > maxMapDB {
			return nil, fmt.Errorf("%s is %d bytes, over %d", path, st.Size(), maxMapDB)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if hotJournal(path + "-journal") {
			last = nil
			hot++
			continue
		}
		if last != nil && bytes.Equal(data, last) {
			return data, nil
		}
		last = data
	}
	if hot == mapReadTries {
		return nil, fmt.Errorf("%s-journal was hot on each of %d reads", path, mapReadTries)
	}
	return nil, fmt.Errorf("%s did not settle in %d reads", path, mapReadTries)
}

func hotJournal(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(journalMagic))
	n, _ := io.ReadFull(f, head)
	return n == len(head) && bytes.Equal(head, journalMagic)
}

func sqliteLookup(db []byte, table, key string) (value []byte, found bool, err error) {
	if len(db) == 0 {
		return nil, false, nil
	}
	f, err := openSQLite(db)
	if err != nil {
		return nil, false, err
	}
	root, err := f.tableRoot(table)
	if err != nil || root == 0 {
		return nil, false, err
	}
	err = f.walk(root, func(rec []byte) (bool, error) {
		cols, err := record(rec)
		if err != nil {
			return false, err
		}
		if len(cols) < 2 {
			return false, nil
		}
		if k, ok := cols[0].text(); !ok || k != key {
			return false, nil
		}
		value, found = append([]byte(nil), cols[1].data...), true
		return true, nil
	})
	return value, found, err
}

type sqliteFile struct {
	data     []byte
	pageSize int
	usable   int
}

func openSQLite(data []byte) (*sqliteFile, error) {
	if len(data) < 100 || string(data[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("not an sqlite file")
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		return nil, fmt.Errorf("page size %d", pageSize)
	}
	if data[18] != 1 || data[19] != 1 {
		return nil, fmt.Errorf("file format %d/%d is not a rollback-journal store, so the file"+
			" may lag its log", data[18], data[19])
	}
	if enc := binary.BigEndian.Uint32(data[56:60]); enc != 1 {
		return nil, fmt.Errorf("text encoding %d is not UTF-8", enc)
	}
	usable := pageSize - int(data[20])
	if usable < 480 {
		return nil, fmt.Errorf("%d usable bytes a page", usable)
	}
	if len(data) < pageSize || len(data)%pageSize != 0 {
		return nil, fmt.Errorf("%d bytes is not whole pages of %d", len(data), pageSize)
	}
	return &sqliteFile{data: data, pageSize: pageSize, usable: usable}, nil
}

func (f *sqliteFile) page(n uint32) ([]byte, error) {
	if n == 0 || uint64(n) > uint64(len(f.data)/f.pageSize) {
		return nil, fmt.Errorf("page %d is outside the file", n)
	}
	start := int(n-1) * f.pageSize
	return f.data[start : start+f.pageSize], nil
}

func (f *sqliteFile) tableRoot(table string) (uint32, error) {
	var root uint32
	err := f.walk(1, func(rec []byte) (bool, error) {
		cols, err := record(rec)
		if err != nil {
			return false, err
		}
		if len(cols) < 4 {
			return false, nil
		}
		kind, _ := cols[0].text()
		name, _ := cols[1].text()
		if kind != "table" || name != table {
			return false, nil
		}
		page, ok := cols[3].integer()
		if !ok || page <= 0 || page > math.MaxUint32 {
			return false, fmt.Errorf("table %q has root page %d", table, page)
		}
		root = uint32(page)
		return true, nil
	})
	return root, err
}

func (f *sqliteFile) walk(root uint32, visit func(record []byte) (stop bool, err error)) error {
	seen := map[uint32]bool{}
	var walk func(n uint32) (bool, error)
	walk = func(n uint32) (bool, error) {
		if seen[n] {
			return false, fmt.Errorf("page %d is reached twice", n)
		}
		seen[n] = true
		p, err := f.page(n)
		if err != nil {
			return false, err
		}
		hdr := 0
		if n == 1 {
			hdr = 100
		}
		kind := p[hdr]
		if kind != 0x05 && kind != 0x0d {
			return false, fmt.Errorf("page %d is of kind %#x, not a table page", n, kind)
		}
		cells := int(binary.BigEndian.Uint16(p[hdr+3:]))
		ptrs := hdr + 8
		if kind == 0x05 {
			ptrs = hdr + 12
		}
		if ptrs+2*cells > f.usable {
			return false, fmt.Errorf("page %d claims %d cells", n, cells)
		}
		for i := 0; i < cells; i++ {
			off := int(binary.BigEndian.Uint16(p[ptrs+2*i:]))
			if off < ptrs+2*cells || off+4 > f.usable {
				return false, fmt.Errorf("page %d cell %d is at %d", n, i, off)
			}
			if kind == 0x05 {
				if stop, err := walk(binary.BigEndian.Uint32(p[off:])); stop || err != nil {
					return stop, err
				}
				continue
			}
			rec, err := f.payload(p, off)
			if err != nil {
				return false, fmt.Errorf("page %d cell %d: %w", n, i, err)
			}
			if stop, err := visit(rec); stop || err != nil {
				return stop, err
			}
		}
		if kind == 0x05 {
			return walk(binary.BigEndian.Uint32(p[hdr+8:]))
		}
		return false, nil
	}
	_, err := walk(root)
	return err
}

func (f *sqliteFile) payload(p []byte, off int) ([]byte, error) {
	u := f.usable
	size, n := varint(p[off:u])
	if n == 0 {
		return nil, errors.New("no payload size")
	}
	off += n
	if _, n = varint(p[off:u]); n == 0 {
		return nil, errors.New("no rowid")
	}
	off += n
	if size > uint64(len(f.data)) {
		return nil, fmt.Errorf("payload of %d bytes", size)
	}
	total := int(size)
	local := total
	if x := u - 35; total > x {
		m := (u-12)*32/255 - 23
		k := m + (total-m)%(u-4)
		local = m
		if k <= x {
			local = k
		}
	}
	if off+local > u {
		return nil, fmt.Errorf("%d local bytes at %d", local, off)
	}
	if local == total {
		return p[off : off+local], nil
	}
	if off+local+4 > u {
		return nil, errors.New("no overflow pointer")
	}
	out := make([]byte, 0, total)
	out = append(out, p[off:off+local]...)
	next := binary.BigEndian.Uint32(p[off+local:])
	seen := map[uint32]bool{}
	for len(out) < total {
		if next == 0 || seen[next] {
			return nil, fmt.Errorf("overflow chain ends at page %d with %d of %d bytes", next, len(out), total)
		}
		seen[next] = true
		op, err := f.page(next)
		if err != nil {
			return nil, err
		}
		next = binary.BigEndian.Uint32(op[:4])
		take := min(u-4, total-len(out))
		out = append(out, op[4:4+take]...)
	}
	return out, nil
}

type column struct {
	kind uint64
	data []byte
}

func record(rec []byte) ([]column, error) {
	hsize, n := varint(rec)
	if n == 0 || hsize > uint64(len(rec)) || int(hsize) < n {
		return nil, errors.New("record header")
	}
	body := int(hsize)
	var cols []column
	for pos := n; pos < int(hsize); {
		kind, n := varint(rec[pos:hsize])
		if n == 0 {
			return nil, errors.New("record serial type")
		}
		pos += n
		size, ok := serialSize(kind)
		if !ok || size > len(rec)-body {
			return nil, fmt.Errorf("record column of type %d", kind)
		}
		cols = append(cols, column{kind: kind, data: rec[body : body+size]})
		body += size
	}
	return cols, nil
}

func serialSize(kind uint64) (int, bool) {
	switch {
	case kind <= 4:
		return int(kind), true
	case kind == 5:
		return 6, true
	case kind == 6 || kind == 7:
		return 8, true
	case kind == 8 || kind == 9:
		return 0, true
	case kind == 10 || kind == 11:
		return 0, false
	case kind-12 > math.MaxInt32:
		return 0, false
	default:
		return int((kind - 12) / 2), true
	}
}

func (c column) text() (string, bool) {
	return string(c.data), c.kind >= 13 && c.kind%2 == 1
}

func (c column) integer() (int64, bool) {
	switch c.kind {
	case 8:
		return 0, true
	case 9:
		return 1, true
	case 1, 2, 3, 4, 5, 6:
		var v int64
		for _, b := range c.data {
			v = v<<8 | int64(b)
		}
		shift := 64 - 8*uint(len(c.data))
		return v << shift >> shift, true
	}
	return 0, false
}

func varint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return v<<8 | uint64(b[i]), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
