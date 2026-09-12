// Package sendspin speaks the Sendspin protocol to a Music Assistant server.
package sendspin

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const wsAcceptSalt = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const maxWSFrames = 4096

type opcode byte

const (
	opContinuation opcode = 0x0
	opText         opcode = 0x1
	opBinary       opcode = 0x2
	opClose        opcode = 0x8
	opPing         opcode = 0x9
	opPong         opcode = 0xa
)

const (
	closeNoStatus = 1005
)

const (
	maxControlPayload = 125
	maxMessage        = 65535
	maxRequestBytes   = 8192
	maxRequestLines   = 64
	wsKeyLen          = 16
)

var (
	errProtocol = errors.New("websocket protocol error")
	errTooBig   = errors.New("websocket message exceeds the read limit")
	errTooMany  = errors.New("websocket message is split across too many frames")
)

type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("websocket closed: %d", e.Code)
	}
	return fmt.Sprintf("websocket closed: %d (%q)", e.Code, e.Reason)
}

type upgradeError struct {
	status int
	text   string
}

func (e *upgradeError) Error() string {
	return fmt.Sprintf("websocket upgrade refused: %d %s", e.status, e.text)
}

type Conn struct {
	c     net.Conn
	r     *bufio.Reader
	limit int
	idle  atomic.Int64

	mu sync.Mutex
}

func Accept(c net.Conn, path string) (*Conn, error) {
	r := bufio.NewReader(c)
	key, err := readUpgrade(r, path)
	if err != nil {
		var ue *upgradeError
		if errors.As(err, &ue) {
			fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nConnection: close\r\n\r\n", ue.status, ue.text)
		}
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsAcceptSalt))
	_, err = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	if err != nil {
		return nil, err
	}
	return &Conn{c: c, r: r, limit: maxMessage}, nil
}

func readUpgrade(r *bufio.Reader, path string) (string, error) {
	line, err := readLine(r)
	if err != nil {
		return "", err
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != "GET" || parts[2] != "HTTP/1.1" {
		return "", &upgradeError{400, "Bad Request"}
	}
	got := parts[1]
	if i := strings.IndexByte(got, '?'); i >= 0 {
		got = got[:i]
	}
	if got != path {
		return "", &upgradeError{404, "Not Found"}
	}
	headers := map[string]string{}
	for n := 0; ; n++ {
		if n >= maxRequestLines {
			return "", &upgradeError{431, "Request Header Fields Too Large"}
		}
		line, err := readLine(r)
		if err != nil {
			return "", err
		}
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return "", &upgradeError{400, "Bad Request"}
		}
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if !strings.EqualFold(headers["upgrade"], "websocket") {
		return "", &upgradeError{400, "Bad Request"}
	}
	if !hasToken(headers["connection"], "upgrade") {
		return "", &upgradeError{400, "Bad Request"}
	}
	if headers["sec-websocket-version"] != "13" {
		return "", &upgradeError{426, "Upgrade Required"}
	}
	key := headers["sec-websocket-key"]
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != wsKeyLen {
		return "", &upgradeError{400, "Bad Request"}
	}
	return key, nil
}

func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\n' {
			break
		}
		if b.Len() >= maxRequestBytes {
			return "", &upgradeError{431, "Request Header Fields Too Large"}
		}
		b.WriteByte(c)
	}
	return strings.TrimSuffix(b.String(), "\r"), nil
}

func hasToken(list, want string) bool {
	for _, t := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

func (w *Conn) setReadLimit(n int) { w.limit = n }

func (w *Conn) setIdle(d time.Duration) { w.idle.Store(int64(d)) }

func (w *Conn) idleFor() time.Duration { return time.Duration(w.idle.Load()) }

type frameHeader struct {
	fin    bool
	op     opcode
	length uint64
	mask   [4]byte
}

func isControl(op opcode) bool { return op >= opClose }

func (w *Conn) readHeader() (frameHeader, error) {
	if d := w.idleFor(); d > 0 {
		if err := w.c.SetReadDeadline(time.Now().Add(d)); err != nil {
			return frameHeader{}, err
		}
	}
	var b [2]byte
	if _, err := io.ReadFull(w.r, b[:]); err != nil {
		return frameHeader{}, err
	}
	if b[0]&0x70 != 0 {
		return frameHeader{}, fmt.Errorf("%w: reserved bits set", errProtocol)
	}
	h := frameHeader{fin: b[0]&0x80 != 0, op: opcode(b[0] & 0x0f)}
	switch n := b[1] & 0x7f; n {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(w.r, e[:]); err != nil {
			return frameHeader{}, err
		}
		h.length = uint64(binary.BigEndian.Uint16(e[:]))
		if h.length < 126 {
			return frameHeader{}, fmt.Errorf("%w: length %d not minimally encoded", errProtocol, h.length)
		}
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(w.r, e[:]); err != nil {
			return frameHeader{}, err
		}
		h.length = binary.BigEndian.Uint64(e[:])
		if h.length&(1<<63) != 0 {
			return frameHeader{}, fmt.Errorf("%w: length has its high bit set", errProtocol)
		}
		if h.length <= 0xffff {
			return frameHeader{}, fmt.Errorf("%w: length %d not minimally encoded", errProtocol, h.length)
		}
	default:
		h.length = uint64(n)
	}
	if isControl(h.op) {
		if !h.fin {
			return frameHeader{}, fmt.Errorf("%w: fragmented control frame", errProtocol)
		}
		if h.length > maxControlPayload {
			return frameHeader{}, fmt.Errorf("%w: control frame of %d bytes", errProtocol, h.length)
		}
	}
	if b[1]&0x80 == 0 {
		return frameHeader{}, fmt.Errorf("%w: unmasked frame from a client", errProtocol)
	}
	if _, err := io.ReadFull(w.r, h.mask[:]); err != nil {
		return frameHeader{}, err
	}
	return h, nil
}

func (w *Conn) readPayload(h frameHeader) ([]byte, error) {
	if h.length > uint64(w.limit) {
		return nil, fmt.Errorf("%w: frame of %d bytes", errTooBig, h.length)
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(w.r, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		payload[i] ^= h.mask[i%4]
	}
	return payload, nil
}

func (w *Conn) Read() (isBinary bool, msg []byte, err error) {
	var kind opcode
	fragmented := false
	frames := 0
	for {
		if frames++; frames > maxWSFrames {
			return false, nil, fmt.Errorf("%w: %d", errTooMany, frames)
		}
		h, err := w.readHeader()
		if err != nil {
			return false, nil, err
		}
		payload, err := w.readPayload(h)
		if err != nil {
			return false, nil, err
		}
		switch h.op {
		case opPing:
			if err := w.write(opPong, payload); err != nil {
				return false, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code, reason, err := parseClose(payload)
			if err != nil {
				return false, nil, err
			}
			_ = w.write(opClose, payload)
			return false, nil, &CloseError{Code: code, Reason: reason}
		case opText, opBinary:
			if fragmented {
				return false, nil, fmt.Errorf("%w: %#x began while a message was in flight", errProtocol, h.op)
			}
			kind = h.op
			msg = payload
		case opContinuation:
			if !fragmented {
				return false, nil, fmt.Errorf("%w: continuation with nothing in flight", errProtocol)
			}
			msg = append(msg, payload...)
		default:
			return false, nil, fmt.Errorf("%w: unknown opcode %#x", errProtocol, h.op)
		}
		if len(msg) > w.limit {
			return false, nil, fmt.Errorf("%w: message of %d bytes", errTooBig, len(msg))
		}
		if h.fin {
			return kind == opBinary, msg, nil
		}
		fragmented = true
	}
}

func parseClose(payload []byte) (int, string, error) {
	switch len(payload) {
	case 0:
		return closeNoStatus, "", nil
	case 1:
		return 0, "", fmt.Errorf("%w: close frame of one byte", errProtocol)
	}
	return int(binary.BigEndian.Uint16(payload[:2])), string(payload[2:]), nil
}

func (w *Conn) write(op opcode, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if d := w.idleFor(); d > 0 {
		if err := w.c.SetWriteDeadline(time.Now().Add(d)); err != nil {
			return err
		}
	}
	first := byte(op) | 0x80
	var head []byte
	switch n := len(payload); {
	case n <= maxControlPayload:
		head = []byte{first, byte(n)}
	case n <= 0xffff:
		head = []byte{first, 126, 0, 0}
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = make([]byte, 10)
		head[0], head[1] = first, 127
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}
	_, err := w.c.Write(append(head, payload...))
	return err
}

func (w *Conn) WriteText(payload []byte) error { return w.write(opText, payload) }

func (w *Conn) WriteBinary(payload []byte) error { return w.write(opBinary, payload) }

func (w *Conn) Ping() error { return w.write(opPing, nil) }
