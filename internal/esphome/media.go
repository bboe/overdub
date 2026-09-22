package esphome

import (
	"fmt"
	"log"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/untrustedlog"
)

// From aioesphomeapi's MediaPlayerEntityFeature.
const (
	featVolumeSet     = 1 << 2
	featVolumeMute    = 1 << 3
	featPlayMedia     = 1 << 9
	featVolumeStep    = 1 << 10
	featMediaAnnounce = 1 << 20
)

const (
	mediaStateIdle    = 1
	mediaStatePlaying = 2

	mediaMute       = 3
	mediaUnmute     = 4
	mediaVolumeUp   = 6
	mediaVolumeDown = 7
)

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

const (
	routeSpeaker   = "speaker"
	routeJack      = "jack"
	routeBluetooth = "bluetooth"
)

func activeRoute(v device.MusicVolume, occupied, jackKnown bool) (string, bool) {
	if !v.BluetoothRouteOK {
		return "", false
	}
	if v.BluetoothRoute {
		return routeBluetooth, true
	}
	if !jackKnown {
		return "", false
	}
	if occupied {
		return routeJack, true
	}
	return routeSpeaker, true
}

func activeVolume(v device.MusicVolume, occupied, jackKnown bool) (step int, percent float32, ok bool) {
	if route, known := activeRoute(v, occupied, jackKnown); known && route == routeBluetooth {
		return v.BluetoothStep, v.Bluetooth, v.BluetoothOK
	}
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
	if s.CanSetVolume() {
		flags |= featVolumeSet | featVolumeStep
	}
	if s.CanMute() {
		flags |= featVolumeMute
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
	s.untrustedLog.Printf("playback: alexa reported a failure: %s", untrustedlog.Cut(detail))
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
			"that plays", conn.sock.RemoteAddr(), untrustedlog.Cut(url))
		return
	}
	conn.noted = fmt.Sprintf("esphome api: %s asked to play %s (announcement=%v)",
		conn.sock.RemoteAddr(), untrustedlog.Cut(url), announcement)

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
			s.untrustedLog.Printf("playback: %s", untrustedlog.Cut(err.Error()))
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
			"found to run it with", conn.sock.RemoteAddr(), untrustedlog.Cut(text))
		return
	}
	if len(s.cmdQueue) >= commandQueue {
		conn.noted = fmt.Sprintf("esphome api: %s asked alexa to run %s, and %d are already "+
			"waiting; it was dropped rather than queued",
			conn.sock.RemoteAddr(), untrustedlog.Cut(text), len(s.cmdQueue))
		return
	}
	if text == "" {
		conn.noted = fmt.Sprintf("esphome api: %s emptied the command box",
			conn.sock.RemoteAddr())
	} else {
		conn.noted = fmt.Sprintf("esphome api: %s asked alexa to run %s",
			conn.sock.RemoteAddr(), untrustedlog.Cut(text))
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
			s.untrustedLog.Printf("alexa command: %v", err)
		}
	}
}

func isFinite(v float32) bool { return !math.IsInf(float64(v), 0) && !math.IsNaN(float64(v)) }

func (s *Server) UseVolumeSetter(set func(step int) error) {
	s.volumeSet = set
}

func (s *Server) UseMute(set func(on bool)) {
	s.muteSet = set
}

func (s *Server) CanMute() bool { return s.muteSet != nil }

func (s *Server) SetSpeakerMute(on bool) {
	if !s.CanMute() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueMuteLocked(on)
}

func (s *Server) setMuteLocked(conn *conn, on bool) {
	if !s.CanMute() {
		conn.noted = fmt.Sprintf("esphome api: %s asked for a mute, and there is nothing"+
			" here to set", conn.sock.RemoteAddr())
		return
	}
	s.queueMuteLocked(on)
	conn.noted = fmt.Sprintf("esphome api: %s set the mute to %v",
		conn.sock.RemoteAddr(), on)
}

func (s *Server) queueMuteLocked(on bool) {
	s.muteWant, s.muteHasPending = on, true
	if s.muteWorking {
		return
	}
	s.muteWorking = true
	go s.muteWorker()
}

func (s *Server) muteWorker() {
	for {
		s.mu.Lock()
		if !s.muteHasPending {
			s.muteWorking = false
			s.mu.Unlock()
			return
		}
		on, set := s.muteWant, s.muteSet
		s.muteHasPending = false
		s.mu.Unlock()

		set(on)

		select {
		case s.liveWake <- struct{}{}:
		default:
		}
	}
}

func (s *Server) setVolumeLocked(conn *conn, want volumeWant) {
	s.queueVolumeLocked(want)
	conn.noted = fmt.Sprintf("esphome api: %s set the volume to %s",
		conn.sock.RemoteAddr(), s.volWant)
}

func (s *Server) queueVolumeLocked(want volumeWant) {
	if s.volHasPending && !want.absolute {
		s.volWant.steps += want.steps
	} else {
		s.volWant = want
	}
	s.volHasPending = true
	if s.volWorking {
		return
	}
	s.volWorking = true
	go s.volumeWorker()
}

func (s *Server) CanSetVolume() bool { return s.volumeSet != nil }

func (s *Server) SpeakerLevel() (percent int, muted, ok bool) {
	step, max, muted, ok := s.readVolumeStep()
	if !ok {
		return 0, false, false
	}
	return int(math.Round(float64(step) * 100 / float64(max))), muted, true
}

func (s *Server) SetSpeakerVolume(percent int) {
	if !s.CanSetVolume() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueVolumeLocked(volumeWant{fraction: float32(percent) / 100, absolute: true})
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
	step, max, _, ok := s.readVolumeStep()
	if !ok {
		s.untrustedLog.Printf("volume: asked for %s, and the level could not be read", want)
		return
	}
	target := clampStep(want.target(step, max), max)
	if target == step {
		return
	}
	s.setStep(target, max, want)
}

func (s *Server) setStep(target, max int, want volumeWant) {
	if s.volumeSet == nil {
		s.untrustedLog.Printf("volume: asked for %s, and there is nothing to set it with", want)
		return
	}
	if err := s.volumeSet(target); err != nil {
		s.untrustedLog.Printf("volume: %v", err)
		return
	}
	switch at, _, _, ok := s.readVolumeStep(); {
	case !ok:
		s.untrustedLog.Printf("volume: asked for step %d of %d, and the level could not be"+
			" read back", target, max)
	case at != target:
		s.untrustedLog.Printf("volume: asked for step %d of %d, device is at %d", target, max, at)
	default:
		s.untrustedLog.Printf("volume: step %d of %d", at, max)
	}
}

func (w volumeWant) target(step, max int) int {
	base := step
	if w.absolute {
		base = int(math.Round(float64(w.fraction) * float64(max)))
	}
	return base + w.steps
}

func (s *Server) readVolumeStep() (step, max int, muted, ok bool) {
	v := s.volumes()
	occupied, jackKnown := s.jack()
	step, _, ok = activeVolume(v, occupied, jackKnown)
	if !ok || v.Max <= 0 {
		return 0, 0, false, false
	}
	return step, v.Max, v.Muted, true
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
