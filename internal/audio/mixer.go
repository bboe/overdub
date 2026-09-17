package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
)

var errStopping = errors.New("audio: the player is closing")

const (
	writeWait  = 2 * time.Millisecond
	stuckWaits = 500
)

const (
	BlockFrames = 480
	BlockBytes  = BlockFrames * 2
)

type source interface {
	read(block []int16) (int, bool)
}

type clip struct {
	pcm []int16
	at  int
}

func (c *clip) read(block []int16) (int, bool) {
	n := copy(block, c.pcm[c.at:])
	c.at += n
	return n, c.at < len(c.pcm)
}

type mixer struct {
	mu      sync.Mutex
	srcs    []source
	scratch []int16

	woke chan struct{}
}

func newMixer() *mixer {
	return &mixer{
		scratch: make([]int16, BlockFrames),
		woke:    make(chan struct{}, 1),
	}
}

func (m *mixer) start(c *clip) {
	m.mu.Lock()
	c.at = 0
	m.mu.Unlock()
	m.add(c)
}

func (m *mixer) add(s source) {
	m.mu.Lock()
	if !slices.Contains(m.srcs, s) {
		m.srcs = append(m.srcs, s)
	}
	m.mu.Unlock()
	select {
	case m.woke <- struct{}{}:
	default:
	}
}

func (m *mixer) sounding() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.srcs) > 0
}

func (m *mixer) next(block []int16) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.srcs) == 0 {
		return false
	}
	if len(m.scratch) < len(block) {
		m.scratch = make([]int16, len(block))
	}
	clear(block)
	keep := m.srcs[:0]
	for _, s := range m.srcs {
		n, more := s.read(m.scratch[:len(block)])
		for i := range n {
			block[i] = sum(block[i], m.scratch[i])
		}
		if more {
			keep = append(keep, s)
		}
	}
	m.srcs = keep
	return true
}

func sum(a, b int16) int16 {
	s := int32(a) + int32(b)
	if s > math.MaxInt16 {
		return math.MaxInt16
	}
	if s < math.MinInt16 {
		return math.MinInt16
	}
	return int16(s)
}

func encode(block []int16, dst []byte) {
	for i, s := range block {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(s))
	}
}

func decode(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

func writeAll(buf []byte, write func([]byte) (int, error), wait func() bool) error {
	refused := 0
	for len(buf) > 0 {
		n, err := write(buf)
		if err != nil {
			return err
		}
		if n < 0 || n > len(buf) {
			return fmt.Errorf("audio: the player took %d bytes of the %d it was offered",
				n, len(buf))
		}
		if n == 0 {
			if refused++; refused > stuckWaits {
				return fmt.Errorf("audio: the player refused a block %d times over %s,"+
					" so its queue has stopped draining", refused, stuckWaits*writeWait)
			}
			if !wait() {
				return errStopping
			}
			continue
		}
		refused = 0
		buf = buf[n:]
	}
	return nil
}
