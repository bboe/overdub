package esphome

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bboe/overdub/internal/device"
)

// From aioesphomeapi's MediaPlayerEntityFeature.
const (
	featVolumeSet     = 1 << 2
	featPlayMedia     = 1 << 9
	featVolumeStep    = 1 << 10
	featMediaAnnounce = 1 << 20
)

const (
	mediaStateIdle    = 1
	mediaStatePlaying = 2

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

func mediaState(key uint32, state uint32, volume float32, muted bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.u32(2, state)
	p.float(3, volume)
	p.boolean(4, muted)
	return p.b
}

func mediaStateFor(playing bool) uint32 {
	if playing {
		return mediaStatePlaying
	}
	return mediaStateIdle
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

func (s *Server) mediaFeatures() uint32 {
	var flags uint32
	if s.volumeKeys != nil {
		flags |= featVolumeSet | featVolumeStep
	}
	if s.play != nil {
		flags |= featPlayMedia | featMediaAnnounce
	}
	return flags
}

func (s *Server) UsePlay(play func(url string) error) {
	s.mu.Lock()
	relist := s.play == nil && play != nil
	s.play = play
	var dropped []string
	if relist {
		for c := range s.conns {
			dropped = append(dropped, c.sock.RemoteAddr().String())
			delete(s.conns, c)
			c.sock.Close()
		}
	}
	s.mu.Unlock()

	for _, addr := range dropped {
		log.Printf("esphome api: dropped %s so it lists the entities again; the media player "+
			"can play now", addr)
	}
}

func (s *Server) NotePlayback(playing bool) {
	s.mu.Lock()
	changed := s.mpPlaying != playing
	s.mpPlaying = playing
	s.mu.Unlock()
	if !changed {
		return
	}
	select {
	case s.liveWake <- struct{}{}:
	default:
	}
}

func (s *Server) NotePlaybackFailed(detail string) {
	if detail == "" {
		detail = "no reason given"
	}
	s.peerLogf("playback: alexa reported a failure: %s", truncate(detail))
	s.NotePlayback(false)
}

func (s *Server) playing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mpPlaying
}

func (s *Server) playLocked(conn *conn, url string, announcement bool) {
	if s.play == nil {
		conn.noted = fmt.Sprintf("esphome api: %s asked to play %s, and there is nothing here "+
			"that plays", conn.sock.RemoteAddr(), truncate(url))
		return
	}
	conn.noted = fmt.Sprintf("esphome api: %s asked to play %s (announcement=%v)",
		conn.sock.RemoteAddr(), truncate(url), announcement)

	s.playWant, s.playHasPending = url, true
	if s.playWorking {
		return
	}
	s.playWorking = true
	go s.playWorker()
}

func (s *Server) playWorker() {
	for {
		s.mu.Lock()
		if !s.playHasPending {
			s.playWorking = false
			s.mu.Unlock()
			return
		}
		url, play := s.playWant, s.play
		s.playHasPending = false
		s.mu.Unlock()

		if play == nil {
			continue
		}
		if err := play(url); err != nil {
			s.peerLogf("playback: %s", truncate(err.Error()))
			s.NotePlayback(false)
		}
	}
}

func (s *Server) UseCommand(send func(text string) error) {
	s.mu.Lock()
	relist := s.command == nil && send != nil
	s.command = send
	if send != nil {
		if _, told := s.published[s.keyText]; !told {
			s.published[s.keyText] = reading{key: s.keyText, ok: true, kind: kindText}
		}
	}
	var dropped []string
	if relist {
		for c := range s.conns {
			dropped = append(dropped, c.sock.RemoteAddr().String())
			delete(s.conns, c)
			c.sock.Close()
		}
	}
	s.mu.Unlock()

	for _, addr := range dropped {
		log.Printf("esphome api: dropped %s so it lists the entities again; alexa commands "+
			"can run now", addr)
	}
}

func (s *Server) hasCommand() bool { return s.command != nil }

func (s *Server) commandLocked(conn *conn, text string) {
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n > commandMaxLength {
		conn.noted = fmt.Sprintf("esphome api: %s asked alexa to run %d characters, and the "+
			"box takes %d; it was dropped rather than sent",
			conn.sock.RemoteAddr(), n, commandMaxLength)
		return
	}
	if s.command == nil {
		conn.noted = fmt.Sprintf("esphome api: %s asked alexa to run %s, and no credential was "+
			"found to run it with", conn.sock.RemoteAddr(), truncate(text))
		return
	}
	if len(s.cmdQueue) >= commandQueue {
		conn.noted = fmt.Sprintf("esphome api: %s asked alexa to run %s, and %d are already "+
			"waiting; it was dropped rather than queued",
			conn.sock.RemoteAddr(), truncate(text), len(s.cmdQueue))
		return
	}
	if text == "" {
		conn.noted = fmt.Sprintf("esphome api: %s emptied the command box",
			conn.sock.RemoteAddr())
	} else {
		conn.noted = fmt.Sprintf("esphome api: %s asked alexa to run %s",
			conn.sock.RemoteAddr(), truncate(text))
	}

	s.cmdQueue = append(s.cmdQueue, text)
	if s.cmdWorking {
		return
	}
	s.cmdWorking = true
	go s.commandWorker()
}

func (s *Server) commandWorker() {
	for {
		s.mu.Lock()
		if len(s.cmdQueue) == 0 {
			s.cmdWorking = false
			s.mu.Unlock()
			return
		}
		text, send := s.cmdQueue[0], s.command
		s.cmdQueue = s.cmdQueue[1:]
		s.mu.Unlock()

		if send == nil {
			continue
		}
		s.publish("command", []reading{{key: s.keyText, text: text, ok: true, kind: kindText}})
		if text == "" {
			continue
		}
		if err := send(text); err != nil {
			s.peerLogf("alexa command: %v", err)
		}
	}
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
