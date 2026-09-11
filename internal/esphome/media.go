package esphome

import (
	"fmt"
	"math"
	"time"

	"github.com/bboe/overdub/internal/device"
)

// From aioesphomeapi's MediaPlayerEntityFeature.
const (
	featVolumeSet  = 1 << 2
	featVolumeStep = 1 << 10
)

const (
	mediaStateIdle = 1

	mediaVolumeUp   = 6
	mediaVolumeDown = 7
)

const volumeSettleFor = 400 * time.Millisecond

type volumeWant struct {
	fraction float32
	absolute bool
	steps    int
}

func (w volumeWant) String() string {
	switch {
	case w.absolute && w.steps != 0:
		return fmt.Sprintf("%.0f%% and %+d steps", w.fraction*100, w.steps)
	case w.absolute:
		return fmt.Sprintf("%.0f%%", w.fraction*100)
	}
	return fmt.Sprintf("%+d steps", w.steps)
}

func mediaState(key uint32, volume float32, muted bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.u32(2, mediaStateIdle)
	p.float(3, volume)
	p.boolean(4, muted)
	return p.b
}

func activeVolume(v device.MusicVolume, occupied, jackKnown bool) (step int, percent float32, ok bool) {
	if !jackKnown {
		return 0, 0, false
	}
	if occupied {
		return v.JackStep, v.Jack, v.JackOK
	}
	return v.SpeakerStep, v.Speaker, v.SpeakerOK
}

func (s *Server) volumeFeatures() uint32 {
	if s.volumeKeys == nil {
		return 0
	}
	return featVolumeSet | featVolumeStep
}

func isFinite(v float32) bool { return !math.IsInf(float64(v), 0) && !math.IsNaN(float64(v)) }

func (s *Server) UseVolumeKeys(step func(up bool, n int) error) {
	s.volumeKeys = step
}

func (s *Server) setVolumeLocked(conn *conn, want volumeWant) {
	if s.volHasPending && !want.absolute {
		s.volWant.steps += want.steps
	} else {
		s.volWant = want
	}
	s.volHasPending = true
	conn.noted = fmt.Sprintf("esphome api: %s set the volume to %s",
		conn.sock.RemoteAddr(), s.volWant)
	if s.volWorking {
		return
	}
	s.volWorking = true
	go s.volumeWorker()
}

func (s *Server) volumeWorker() {
	for {
		s.mu.Lock()
		if !s.volHasPending {
			s.volWorking = false
			s.mu.Unlock()
			return
		}
		want := s.volWant
		s.volHasPending = false
		s.mu.Unlock()

		s.setVolume(want)
	}
}

func (s *Server) setVolume(want volumeWant) {
	s.applyVolume(want)

	select {
	case s.liveWake <- struct{}{}:
	default:
	}
}

func (s *Server) applyVolume(want volumeWant) {
	step, max, ok := s.readVolumeStep()
	if !ok {
		s.peerLogf("volume: asked for %s, and the level could not be read", want)
		return
	}
	target := clampStep(want.target(step, max), max)
	delta := target - step
	if delta == 0 {
		return
	}
	if s.volumeKeys == nil {
		s.peerLogf("volume: asked for %s, and there are no keys to press", want)
		return
	}
	if err := s.volumeKeys(delta > 0, abs(delta)); err != nil {
		s.peerLogf("volume: %v", err)
		return
	}
	time.Sleep(s.volumeSettle)

	landed, _, ok := s.readVolumeStep()
	switch {
	case !ok:
		s.peerLogf("volume: asked for step %d of %d, and the level could not be read back",
			target, max)
	case landed != target:
		s.peerLogf("volume: asked for step %d of %d, device is at %d", target, max, landed)
	default:
		s.peerLogf("volume: step %d of %d", landed, max)
	}
}

func (w volumeWant) target(step, max int) int {
	base := step
	if w.absolute {
		base = int(math.Round(float64(w.fraction) * float64(max)))
	}
	return base + w.steps
}

func (s *Server) readVolumeStep() (step, max int, ok bool) {
	v := s.volumes()
	occupied, jackKnown := s.jack()
	step, _, ok = activeVolume(v, occupied, jackKnown)
	if !ok || v.Max <= 0 {
		return 0, 0, false
	}
	return step, v.Max, true
}

func clampStep(step, max int) int {
	if step < 0 {
		return 0
	}
	if step > max {
		return max
	}
	return step
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
