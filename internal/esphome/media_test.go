package esphome

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
)

type fakeVolume struct {
	mu        sync.Mutex
	v         device.MusicVolume
	occupied  bool
	jackKnown bool
	sets      int
	err       error
}

func wireFakeVolume(s *Server, step, max int) *fakeVolume {
	f := &fakeVolume{jackKnown: true}
	f.v = device.MusicVolume{Max: max, SpeakerStep: step, SpeakerOK: true}
	f.v.Speaker = stepPercentFor(step, max)
	s.volumes = func() device.MusicVolume {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.v
	}
	s.jack = func() (bool, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.occupied, f.jackKnown
	}
	s.volumeSet = func(step int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sets++
		if f.err != nil {
			return f.err
		}
		if f.occupied {
			f.v.JackStep = step
			f.v.Jack = stepPercentFor(step, f.v.Max)
		} else {
			f.v.SpeakerStep = step
			f.v.Speaker = stepPercentFor(step, f.v.Max)
		}
		return nil
	}
	return f
}

func stepPercentFor(step, max int) float32 { return float32(step) * 100 / float32(max) }

func (f *fakeVolume) state() (speaker, jack, sets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v.SpeakerStep, f.v.JackStep, f.sets
}

func waitVolumeIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.volWorking || s.volHasPending
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the volume worker never went idle")
}

func askVolume(s *Server, want volumeWant) {
	s.mu.Lock()
	s.setVolumeLocked(&conn{sock: fakeAddr{}}, want)
	s.mu.Unlock()
}

func TestAVolumeSetStepsToTheLevelAskedFor(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	speaker, _, sets := f.state()
	if speaker != 15 || sets != 1 {
		t.Errorf("half of thirty left the speaker at %d after %d sets, want 15 from one",
			speaker, sets)
	}
}

func TestAVolumeAlreadyWhereItWasAskedSetsNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 15, 30)

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	if _, _, sets := f.state(); sets != 0 {
		t.Errorf("a volume already at the level asked for was set %d times", sets)
	}
}

func TestVolumeUpAndDownMoveOneStep(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	askVolume(s, volumeWant{steps: 1})
	waitVolumeIdle(t, s)
	if speaker, _, sets := f.state(); speaker != 7 || sets != 1 {
		t.Errorf("one step up left the speaker at %d after %d sets, want 7 after 1",
			speaker, sets)
	}

	askVolume(s, volumeWant{steps: -1})
	waitVolumeIdle(t, s)
	if speaker, _, sets := f.state(); speaker != 6 || sets != 2 {
		t.Errorf("one step down left the speaker at %d after %d sets, want 6 after 2",
			speaker, sets)
	}
}

func TestTheVolumeIsClampedToTheScale(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	for _, tt := range []struct {
		name  string
		start int
		want  volumeWant
		end   int
	}{
		{"above the top", 6, volumeWant{fraction: 1.5, absolute: true}, 30},
		{"below the bottom", 6, volumeWant{fraction: -0.5, absolute: true}, 0},
		{"a step up from the top stays there", 30, volumeWant{steps: 1}, 30},
		{"a step down from the bottom stays there", 0, volumeWant{steps: -1}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, testPSK(t))
			f := wireFakeVolume(s, tt.start, 30)

			askVolume(s, tt.want)
			waitVolumeIdle(t, s)

			if speaker, _, _ := f.state(); speaker != tt.end {
				t.Errorf("the speaker landed at %d, want %d", speaker, tt.end)
			}
		})
	}
}

func TestAnUnreadableVolumeIsNotStepped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	f.v.SpeakerOK = false

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	if _, _, sets := f.state(); sets != 0 {
		t.Errorf("a level that could not be read was set %d times: a level computed "+
			"from an unknown one lands somewhere nobody asked for", sets)
	}
}

func TestTheVolumeFollowsTheRouteThatIsLive(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	f.occupied = true
	f.v.JackStep, f.v.JackOK = 9, true
	f.v.Jack = stepPercentFor(9, 30)

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	speaker, jack, sets := f.state()
	if jack != 15 || sets != 1 {
		t.Errorf("with a plug in the socket the jack went to %d after %d sets, "+
			"want 15 from one set: the speaker's own step is not the one that moves",
			jack, sets)
	}
	if speaker != 6 {
		t.Errorf("the speaker's step moved to %d while the jack was the live route", speaker)
	}
}

func TestStepsAskedForTogetherAreBothTaken(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	c := &conn{sock: fakeAddr{}}
	s.mu.Lock()
	s.setVolumeLocked(c, volumeWant{steps: 1})
	s.setVolumeLocked(c, volumeWant{steps: 1})
	s.mu.Unlock()
	waitVolumeIdle(t, s)

	if speaker, _, _ := f.state(); speaker != 8 {
		t.Errorf("two steps up left the speaker at %d, want 8: a second step arriving "+
			"before the first is served is not a replacement", speaker)
	}
}

func TestASetThatFailedIsNotReportedAsAVolume(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	f.err = errors.New("service call: transaction failed")

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	if speaker, _, _ := f.state(); speaker != 6 {
		t.Errorf("a failed set moved the speaker to %d", speaker)
	}
}

func TestAMediaCommandForAnotherKeyIsIgnored(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, volumeCommand(s.keySound, 0.5)); err != nil {
		t.Fatalf("a media command for another key was an error: %v", err)
	}
	waitVolumeIdle(t, s)
	if _, _, sets := f.state(); sets != 0 {
		t.Errorf("a command naming the speaker sensor set the volume %d times", sets)
	}

	if err := s.handle(c, msgMediaPlayerCmd, volumeCommand(s.keySpeaker, 0.5)); err != nil {
		t.Fatalf("a media command for the speaker was an error: %v", err)
	}
	waitVolumeIdle(t, s)
	if speaker, _, sets := f.state(); speaker != 15 || sets != 1 {
		t.Errorf("the speaker is at %d after %d sets, want 15 after 1", speaker, sets)
	}
}

func volumeCommand(key uint32, volume float32) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(4, true)
	p.float(5, volume)
	return p.b
}

func TestTheVolumeWakesThePollThatReadsIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	wireFakeVolume(s, 6, 30)

	askVolume(s, volumeWant{steps: 1})
	waitVolumeIdle(t, s)

	if len(s.liveWake) != 1 {
		t.Error("the live poll was not woken, and it is the one that reads the volume")
	}
}

func TestActiveVolumeIsTheRouteInUse(t *testing.T) {
	full := device.MusicVolume{
		Max: 30, Speaker: 40, SpeakerStep: 12, SpeakerOK: true,
		Jack: 70, JackStep: 21, JackOK: true,
	}
	for _, tt := range []struct {
		name      string
		v         device.MusicVolume
		occupied  bool
		jackKnown bool
		step      int
		percent   float32
		ok        bool
	}{
		{"nothing in the socket", full, false, true, 12, 40, true},
		{"something in the socket", full, true, true, 21, 70, true},
		{"a socket we cannot read is not a route we can count from", full, true, false, 0, 0, false},
		{"a socket we cannot read, with nothing plugged in either", full, false, false, 0, 0, false},
		{"the live route has no level",
			device.MusicVolume{Max: 30, Speaker: 40, SpeakerStep: 12, SpeakerOK: true},
			true, true, 0, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			step, percent, ok := activeVolume(tt.v, tt.occupied, tt.jackKnown)
			if step != tt.step || percent != tt.percent || ok != tt.ok {
				t.Errorf("active = %d, %v, %v; want %d, %v, %v",
					step, percent, ok, tt.step, tt.percent, tt.ok)
			}
		})
	}
}

func TestTheSpeakerIsListedAsAMediaPlayerThatOnlyDoesVolume(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.UseVolumeSetter(func(int) error { return nil })
	for _, entity := range listed(t, s) {
		if entity[0].num != uint64(msgListMediaPlayer) {
			continue
		}
		if got := string(entity[1].data); got != "speaker" {
			t.Errorf("the media player is object_id %q, want %q", got, "speaker")
		}
		if got := entity[11].num; got != featVolumeSet|featVolumeStep {
			t.Errorf("feature_flags is %d, want %d: this server was given a setter and no player, "+
				"and a control that does nothing is worse than one that is absent",
				got, featVolumeSet|featVolumeStep)
		}
		return
	}
	t.Error("no media player was listed, so Home Assistant has no volume control")
}

func TestTheStateCarriesTheVolumeAndTheMute(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  uint32
		volume float32
		muted  bool
	}{
		{"turned down", mediaStateIdle, 0, true},
		{"audible", mediaStateIdle, 0.2, false},
		{"muted at a level it will return to", mediaStateIdle, 0.4, true},
		{"playing", mediaStatePlaying, 0.4, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fields := map[int]pbField{}
			if err := pbWalk(mediaState(7, tt.state, tt.volume, tt.muted), func(f pbField) { fields[f.field] = f }); err != nil {
				t.Fatalf("the state did not parse: %v", err)
			}
			if got := uint32(fields[2].num); got != tt.state {
				t.Errorf("state is %d, want %d", got, tt.state)
			}
			if got := math.Float32frombits(uint32(fields[3].num)); got != tt.volume {
				t.Errorf("volume is %v, want %v", got, tt.volume)
			}
			if got := fields[4].num != 0; got != tt.muted {
				t.Errorf("muted = %v, want %v", got, tt.muted)
			}
		})
	}
}

func TestAMutedStreamPublishesTheLevelItWillReturnTo(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.jack = func() (bool, bool) { return false, true }
	s.volumes = func() device.MusicVolume {
		return device.MusicVolume{Max: 30, Muted: true, SpeakerStep: 12, SpeakerOK: true, Speaker: 0}
	}

	var media reading
	found := false
	for _, r := range s.readLive() {
		if r.key == s.keySpeaker {
			media, found = r, true
		}
	}
	if !found {
		t.Fatal("a muted stream published no media state at all")
	}
	if media.value != 0.4 || !media.muted {
		t.Errorf("a muted stream published volume %v muted %v, want 0.4 and muted: the "+
			"slider has to sit where a set counts from, or every set lands on the device "+
			"and snaps back in Home Assistant", media.value, media.muted)
	}
}

func TestAStepOnTopOfAPendingSetIsAddedToIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	c := &conn{sock: fakeAddr{}}
	s.mu.Lock()
	s.setVolumeLocked(c, volumeWant{fraction: 0.5, absolute: true})
	s.setVolumeLocked(c, volumeWant{steps: 1})
	s.mu.Unlock()
	waitVolumeIdle(t, s)

	if speaker, _, _ := f.state(); speaker != 16 {
		t.Errorf("a step arriving on a pending set left the speaker at %d, want 16: the"+
			" step belongs on top of the level asked for, not instead of it", speaker)
	}
}

func TestALevelTurnedAllTheWayDownIsNotAMute(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.jack = func() (bool, bool) { return false, true }
	s.volumes = func() device.MusicVolume {
		return device.MusicVolume{Max: 30, SpeakerStep: 0, SpeakerOK: true, Speaker: 0}
	}

	for _, r := range s.readLive() {
		if r.key != s.keySpeaker {
			continue
		}
		if r.muted {
			t.Error("a level stepped down to zero was reported as muted, and nothing here " +
				"declares VOLUME_MUTE, so Home Assistant would show a mute with no way to lift it")
		}
		return
	}
	t.Fatal("no media state was published")
}

func TestASetOnTopOfAPendingStepReplacesIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	c := &conn{sock: fakeAddr{}}
	s.mu.Lock()
	s.setVolumeLocked(c, volumeWant{steps: 1})
	s.setVolumeLocked(c, volumeWant{fraction: 0.5, absolute: true})
	s.mu.Unlock()
	waitVolumeIdle(t, s)

	if speaker, _, _ := f.state(); speaker != 15 {
		t.Errorf("a set arriving on a pending step left the speaker at %d, want 15: a set"+
			" names where to be, so it answers the step rather than landing above it",
			speaker)
	}
}

func TestAnUnreadableRouteIsNotStepped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	f.occupied, f.jackKnown = true, false
	f.v.JackStep, f.v.JackOK = 21, true
	f.v.Jack = stepPercentFor(21, 30)

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	if _, jack, sets := f.state(); sets != 0 {
		t.Errorf("a socket we could not read was set %d times, leaving the jack at %d:"+
			" a level set from an unknown route lands somewhere nobody asked for",
			sets, jack)
	}
}

func TestEveryControlOfferedHasSomethingBehindIt(t *testing.T) {
	s := testServer(t, testPSK(t))
	if got := s.mediaFeatures(); got != 0 {
		t.Errorf("a server with neither a setter nor a player offered feature_flags %d, want none: "+
			"a control that cannot act is worse than a card without one", got)
	}

	s.UseVolumeSetter(func(int) error { return nil })
	if got := s.mediaFeatures(); got != featVolumeSet|featVolumeStep {
		t.Errorf("with a setter and no player feature_flags is %d, want the volume alone (%d)",
			got, featVolumeSet|featVolumeStep)
	}

	s.UsePlay(func(string) error { return nil })
	want := uint32(featVolumeSet | featVolumeStep | featPlayMedia | featMediaAnnounce)
	if got := s.mediaFeatures(); got != want {
		t.Errorf("feature_flags is %d, want %d", got, want)
	}
}

func TestAVolumeThatIsNotAFloatIsNotASet(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	for _, tt := range []struct {
		name    string
		payload []byte
	}{
		{"a varint where a float belongs", func() []byte {
			var p pb
			p.fixed32(1, 0)
			p.boolean(4, true)
			p.u32(5, 0x7fc00000)
			return p.b
		}()},
		{"a float that is not a number", func() []byte {
			var p pb
			p.fixed32(1, 0)
			p.boolean(4, true)
			p.float(5, float32(math.NaN()))
			return p.b
		}()},
		{"a float that is infinite", func() []byte {
			var p pb
			p.fixed32(1, 0)
			p.boolean(4, true)
			p.float(5, float32(math.Inf(1)))
			return p.b
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, testPSK(t))
			f := wireFakeVolume(s, 6, 30)

			payload := append([]byte{}, tt.payload...)
			var key pb
			key.fixed32(1, s.keySpeaker)
			copy(payload, key.b)

			c := &conn{sock: fakeAddr{}}
			if err := s.handle(c, msgMediaPlayerCmd, payload); err != nil {
				t.Fatalf("the command was an error: %v", err)
			}
			waitVolumeIdle(t, s)
			if speaker, _, sets := f.state(); speaker != 6 || sets != 0 {
				t.Errorf("the speaker moved to %d after %d sets", speaker, sets)
			}
		})
	}
}

func playCommand(key uint32, url string, announcement bool) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(6, true)
	p.str(7, url)
	if announcement {
		p.boolean(9, true)
	}
	return p.b
}

func wireFakePlay(s *Server, err error) (*[]string, func()) {
	var mu sync.Mutex
	var asked []string
	done := make(chan struct{}, 8)
	s.UsePlay(func(url string) error {
		mu.Lock()
		asked = append(asked, url)
		mu.Unlock()
		done <- struct{}{}
		return err
	})
	return &asked, func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func TestAPlayCommandHandsTheURLOnUntouched(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	asked, wait := wireFakePlay(s, nil)

	const url = "http://192.168.2.5:8123/local/dot-tts/3-abc.mp3"
	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, playCommand(s.keySpeaker, url, true)); err != nil {
		t.Fatalf("a play command was an error: %v", err)
	}
	wait()

	if len(*asked) != 1 || (*asked)[0] != url {
		t.Errorf("the player was asked for %v, want exactly %q: whatever the url means is "+
			"Alexa's question, not ours", *asked, url)
	}
}

func TestAPlayCommandForAnotherKeyIsNotOurs(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	asked, _ := wireFakePlay(s, nil)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, playCommand(s.keySound, "http://x.invalid/a.mp3", false)); err != nil {
		t.Fatalf("a play command for another key was an error: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(*asked) != 0 {
		t.Errorf("a command naming the speaker sensor played %v", *asked)
	}
}

func TestAnEmptyURLIsNotAPlay(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	asked, _ := wireFakePlay(s, nil)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, playCommand(s.keySpeaker, "", false)); err != nil {
		t.Fatalf("an empty url was an error: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(*asked) != 0 {
		t.Errorf("an empty url was played as %v", *asked)
	}
}

func TestPlaybackIsReportedWhenItIsHeardRatherThanWhenItIsAskedFor(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	_, wait := wireFakePlay(s, nil)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, playCommand(s.keySpeaker, "http://x.invalid/a.mp3", false)); err != nil {
		t.Fatalf("a play command was an error: %v", err)
	}
	wait()
	if s.playing() {
		t.Error("the player reported playing because a peer asked it to: nothing had reached " +
			"the speaker yet, and a state nobody observed is a guess")
	}

	s.NotePlayback(true)
	if !s.playing() {
		t.Error("a playback Alexa reported was not passed on")
	}
	s.NotePlayback(false)
	if s.playing() {
		t.Error("a playback that ended was still reported as playing")
	}
}

func TestAPlayThatCouldNotStartLeavesNothingPlaying(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	_, wait := wireFakePlay(s, errors.New("am startservice: Error: Not found"))
	s.NotePlayback(true)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, playCommand(s.keySpeaker, "http://x.invalid/a.mp3", false)); err != nil {
		t.Fatalf("a play command was an error: %v", err)
	}
	wait()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && s.playing(); {
		time.Sleep(time.Millisecond)
	}
	if s.playing() {
		t.Error("a play that failed to start left the player reporting playing for ever")
	}
}

func TestPlaybackWakesThePollThatPublishesIt(t *testing.T) {
	s := testServer(t, testPSK(t))
	s.NotePlayback(true)
	if len(s.liveWake) != 1 {
		t.Fatal("a playback did not wake the poll, so Home Assistant hears about it on the " +
			"next tick rather than now")
	}
	<-s.liveWake

	s.NotePlayback(true)
	if len(s.liveWake) != 0 {
		t.Error("a playback already reported woke the poll again")
	}
}

func TestTheReadingCarriesWhateverIsPlaying(t *testing.T) {
	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.NotePlayback(true)

	for _, r := range s.readLive() {
		if r.key != s.keySpeaker {
			continue
		}
		if r.state != mediaStatePlaying {
			t.Errorf("the media reading carried state %d while something was playing, want %d",
				r.state, mediaStatePlaying)
		}
		return
	}
	t.Fatal("no media state was published")
}

func TestAFailureAlexaReportsGoesThroughTheLimitedLog(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.NotePlayback(true)
	s.NotePlaybackFailed("cannot estimate length of the next mp3 frame")

	if s.playing() {
		t.Error("a failure left the player reporting playing")
	}
	if got := out.String(); !strings.Contains(got, "cannot estimate length") {
		t.Errorf("the log said %q, want the reason Alexa gave", got)
	}

	s.NotePlayback(true)
	s.NotePlaybackFailed("")
	if got := out.String(); !strings.Contains(got, "no reason given") {
		t.Errorf("a failure with no detail said %q, want it said so", got)
	}
}

func TestAFailureDetailIsCutBeforeItIsLogged(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.NotePlaybackFailed(strings.Repeat("x", 4096))

	if n := len(out.String()); n > 512 {
		t.Errorf("one failure wrote %d bytes to a log that lives on /data: a line Alexa "+
			"prints is still a line a peer asked for", n)
	}
}

func TestPlaysDoNotPileUp(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	var mu sync.Mutex
	inFlight, most, calls := 0, 0, 0
	release := make(chan struct{})
	s.UsePlay(func(string) error {
		mu.Lock()
		inFlight++
		calls++
		if inFlight > most {
			most = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})

	c := &conn{sock: fakeAddr{}}
	s.mu.Lock()
	for i := 0; i < 8; i++ {
		s.playLocked(c, "http://x.invalid/a.mp3", false)
	}
	s.mu.Unlock()

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.playWorking || s.playHasPending
		s.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if most > 1 {
		t.Errorf("%d plays ran at once; each one forks a VM on the Dot, and the volume beside "+
			"it deliberately runs one worker", most)
	}
	if calls == 0 {
		t.Error("eight play commands ran nothing at all")
	}
}

func TestAPlayErrorIsCutBeforeItIsLogged(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	_, wait := wireFakePlay(s, errors.New(strings.Repeat("z", 8192)))

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, playCommand(s.keySpeaker, "http://x.invalid/a.mp3", false)); err != nil {
		t.Fatalf("a play command was an error: %v", err)
	}
	wait()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if n := len(out.String()); n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if n := len(out.String()); n > 512 {
		t.Errorf("one failed play wrote %d bytes to the log", n)
	}
}

func TestThePlayerCanBeWiredWhilePlaysArrive(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	s.UsePlay(func(string) error { return nil })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.UsePlay(func(string) error { return nil })
		}
	}()
	go func() {
		defer wg.Done()
		c := &conn{sock: fakeAddr{}}
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			s.playLocked(c, "http://x.invalid/a.mp3", false)
			s.mu.Unlock()
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		s.mu.Lock()
		busy := s.playWorking || s.playHasPending
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the play worker never went idle")
}

func TestAPlayerThatArrivesLateIsListedAnyway(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.UseVolumeSetter(func(int) error { return nil })

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	s.UsePlay(func(string) error { return nil })

	s.mu.Lock()
	left := len(s.conns)
	s.mu.Unlock()
	if left != 0 {
		t.Errorf("%d connections kept after the player arrived; ListEntities is answered once "+
			"per connection, so a peer that listed before this holds feature_flags with no "+
			"PLAY_MEDIA for the life of the socket", left)
	}

	s.UsePlay(func(string) error { return nil })
	if got := out.String(); strings.Count(got, "lists the entities again") != 1 {
		t.Errorf("the log said %q; wiring a player that was already there drops nobody", got)
	}
}

func TestAVolumeCommandCarryingNoVolumeAtAllIsZero(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	var bare pb
	bare.fixed32(1, s.keySpeaker)
	bare.boolean(4, true)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, bare.b); err != nil {
		t.Fatalf("a media command carrying no volume was an error: %v", err)
	}
	waitVolumeIdle(t, s)
	if speaker, _, sets := f.state(); speaker != 0 || sets == 0 {
		t.Errorf("a command asking for volume 0 left the speaker at %d after %d sets,"+
			" want 0: proto3 leaves a zero-valued scalar off the wire, and has_volume"+
			" is the presence flag that says one was meant", speaker, sets)
	}
}

func TestASetTheDeviceDidNotTakeIsReportedRatherThanRepeated(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	s.volumeSet = func(int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sets++
		return nil
	}

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	if speaker, _, sets := f.state(); speaker != 6 || sets != 1 {
		t.Errorf("a set the device ignored left the speaker at %d after %d sets, want 6"+
			" after 1: a transaction number this build does not share must report"+
			" rather than be tried again", speaker, sets)
	}
	if got := out.String(); !strings.Contains(got, "device is at 6") {
		t.Errorf("the log says %q, and nothing in it names the level the device kept", got)
	}
}

func TestTheVolumeCrossesToSendspinAsAPercentage(t *testing.T) {
	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)

	if !s.CanSetVolume() {
		t.Fatal("a server wired to set a level says it cannot, so sendspin offers no volume")
	}
	if got, muted, ok := s.SpeakerLevel(); !ok || got != 20 || muted {
		t.Errorf("SpeakerLevel is %d muted=%v (ok=%v), want 20 unmuted: step 6 of 30 is"+
			" what sendspin reports, and 0-100 is the only scale the protocol has",
			got, muted, ok)
	}

	s.SetSpeakerVolume(50)
	waitVolumeIdle(t, s)

	if speaker, _, sets := f.state(); speaker != 15 || sets != 1 {
		t.Errorf("a volume a server set left the speaker at %d after %d sets, want 15"+
			" after 1", speaker, sets)
	}
}

func TestAServerCannotSetALevelThereIsNothingToSetWith(t *testing.T) {
	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	s.volumeSet = nil

	if s.CanSetVolume() {
		t.Error("a server with nothing to set the level with says it can, so sendspin" +
			" offers a volume command that reaches nothing")
	}
	s.SetSpeakerVolume(50)
	waitVolumeIdle(t, s)
	if speaker, _, _ := f.state(); speaker != 6 {
		t.Errorf("the speaker moved to %d with nothing wired to move it", speaker)
	}
}

func wireFakeMute(s *Server, f *fakeVolume) *int {
	calls := 0
	s.muteSet = func(on bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		calls++
		f.v.Muted = on
	}
	return &calls
}

func waitMuteIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.muteWorking || s.muteHasPending
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the mute worker never went idle")
}

func TestTheMuteIsOfferedOnlyWhenThereIsAMuteToSet(t *testing.T) {
	s := testServer(t, testPSK(t))
	wireFakeVolume(s, 6, 30)
	if got := s.mediaFeatures(); got&featVolumeMute != 0 {
		t.Errorf("feature_flags %d offers a mute with nothing behind it; home assistant"+
			" draws a control that cannot act", got)
	}
	wireFakeMute(s, &fakeVolume{})
	if got := s.mediaFeatures(); got&featVolumeMute == 0 {
		t.Errorf("feature_flags %d offers no mute on a dot that can mute", got)
	}
}

func muteWireCommand(key uint32, command uint64) []byte {
	var p pb
	p.fixed32(1, key)
	p.boolean(2, true)
	p.u32(3, uint32(command))
	return p.b
}

func TestAMuteCommandOffTheWireReachesTheDevice(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command uint64
		want    bool
	}{
		{"mute", mediaMute, true},
		{"unmute", mediaUnmute, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, testPSK(t))
			f := wireFakeVolume(s, 6, 30)
			wireFakeMute(s, f)
			f.v.Muted = !tt.want

			c := &conn{sock: fakeAddr{}}
			if err := s.handle(c, msgMediaPlayerCmd, muteWireCommand(s.keySpeaker, tt.command)); err != nil {
				t.Fatalf("a %s command was an error: %v", tt.name, err)
			}
			waitMuteIdle(t, s)

			f.mu.Lock()
			defer f.mu.Unlock()
			if f.v.Muted != tt.want {
				t.Errorf("the device is muted=%v after a %s command off the wire, want %v",
					f.v.Muted, tt.name, tt.want)
			}
		})
	}
}

func TestAMuteCommandReachesTheDevice(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command uint64
		want    bool
	}{
		{"mute", mediaMute, true},
		{"unmute", mediaUnmute, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, testPSK(t))
			f := wireFakeVolume(s, 6, 30)
			calls := wireFakeMute(s, f)
			f.v.Muted = !tt.want

			c := &conn{sock: fakeAddr{}}
			s.mu.Lock()
			s.setMuteLocked(c, tt.want)
			s.mu.Unlock()
			waitMuteIdle(t, s)

			f.mu.Lock()
			got := f.v.Muted
			f.mu.Unlock()
			if got != tt.want || *calls != 1 {
				t.Errorf("the device is muted=%v after %d calls, want %v after 1",
					got, *calls, tt.want)
			}
		})
	}
}

func TestTwoMutesArrivingTogetherLeaveTheLastOne(t *testing.T) {
	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	wireFakeMute(s, f)

	c := &conn{sock: fakeAddr{}}
	s.mu.Lock()
	s.setMuteLocked(c, true)
	s.setMuteLocked(c, false)
	s.mu.Unlock()
	waitMuteIdle(t, s)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.v.Muted {
		t.Error("a mute and an unmute arriving together left the dot muted; the second" +
			" names where to be, so it answers the first rather than queueing behind it")
	}
}

func TestTheMuteCrossesOnTheSameReadAsTheLevel(t *testing.T) {
	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	reads := 0
	inner := s.volumes
	s.volumes = func() device.MusicVolume {
		reads++
		return inner()
	}
	f.v.Muted = true

	percent, muted, ok := s.SpeakerLevel()
	if !ok || !muted || percent != 20 {
		t.Errorf("SpeakerLevel is %d muted=%v (ok=%v), want 20 muted", percent, muted, ok)
	}
	if reads != 1 {
		t.Errorf("the level and the mute cost %d reads of the dump, want 1: each one"+
			" forks dumpsys, and the poll that asks for both runs on a tick shorter"+
			" than two of them", reads)
	}

	f.v.SpeakerOK, f.v.JackOK = false, false
	if _, _, ok := s.SpeakerLevel(); ok {
		t.Error("a dump with no readable route still answered, so sendspin reports a guess")
	}
}

func TestAVolumeSetWhileMutedLandsAndStaysMuted(t *testing.T) {
	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	wireFakeMute(s, f)
	f.v.Muted = true

	askVolume(s, volumeWant{fraction: 0.5, absolute: true})
	waitVolumeIdle(t, s)

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.v.Muted {
		t.Error("a level set from home assistant lifted the mute; only the keys on the" +
			" dot do that, and a slider that unmutes makes the mute unholdable")
	}
	if f.v.SpeakerStep != 15 {
		t.Errorf("the level landed on %d, want 15: the dump keeps the step a mute is"+
			" holding, so a set while muted counts from the right number", f.v.SpeakerStep)
	}
}

func TestAVolumeStepWhileMutedCountsFromTheLevelTheMuteHolds(t *testing.T) {
	s := testServer(t, testPSK(t))
	f := wireFakeVolume(s, 6, 30)
	wireFakeMute(s, f)
	f.v.Muted = true

	askVolume(s, volumeWant{steps: 1})
	waitVolumeIdle(t, s)

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.v.Muted {
		t.Error("a step from home assistant lifted the mute")
	}
	if f.v.SpeakerStep != 7 {
		t.Errorf("a step while muted left the level at %d, want 7: the mute zeroes the"+
			" percentage and not the step, so a step counts from 6 rather than 0",
			f.v.SpeakerStep)
	}
}

func TestAMuteOffTheWireWithNothingToSetItDoesNotPanic(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	wireFakeVolume(s, 6, 30)

	c := &conn{sock: fakeAddr{}}
	if err := s.handle(c, msgMediaPlayerCmd, muteWireCommand(s.keySpeaker, mediaMute)); err != nil {
		t.Fatalf("a mute command with no mute wired was an error: %v", err)
	}
	waitMuteIdle(t, s)

	if !strings.Contains(c.noted, "nothing") {
		t.Errorf("the line says %q, and nothing in it says the mute reached nothing:"+
			" a peer holding the key can send command 3 whatever the feature flags"+
			" say, and the worker would call a setter that is not there", c.noted)
	}
}
