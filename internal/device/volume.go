package device

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	audiod      = "com.doppler.audiod"
	puffin      = "com.amazon.puffin"
	VolumeSteps = 30
)

var (
	lipcGetPath     = "/system/bin/lipc-get-prop"
	lipcSetPath     = "/system/bin/lipc-set-prop"
	volumeWatchArgv = []string{"/system/bin/lipc-wait-event", "-m", audiod, "MainVolumeChanged,MuteChanged"}

	lipcTimeout   = time.Second
	lipcWaitDelay = 100 * time.Millisecond
	volumeRetry   = 10 * time.Second
	volumeReread  = time.Minute
	volumeSettle  = time.Second

	lipcCommand = func(ctx context.Context, argv []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.WaitDelay = lipcWaitDelay
		return cmd.Output()
	}
)

type MusicVolume struct {
	Max     int
	Muted   bool
	Percent float32
	Step    int
	OK      bool
}

var volume struct {
	mu       sync.Mutex
	level    int
	levelOK  bool
	muted    bool
	muteOK   bool
	levelGen uint64
	muteGen  uint64
	changed  chan struct{}
}

func MusicVolumes() MusicVolume {
	volume.mu.Lock()
	defer volume.mu.Unlock()
	if !volume.levelOK || !volume.muteOK {
		return MusicVolume{}
	}
	v := MusicVolume{
		Max:     VolumeSteps,
		Muted:   volume.muted,
		Percent: float32(volume.level),
		Step:    levelStep(volume.level),
		OK:      true,
	}
	if v.Muted {
		v.Percent = 0
	}
	return v
}

func levelStep(level int) int { return int(math.Round(float64(level) * VolumeSteps / 100)) }

func stepLevel(step int) int { return int(math.Round(float64(step) * 100 / VolumeSteps)) }

func clampLevel(level int) int { return min(max(level, 0), 100) }

func SetMusicVolume(step int) error {
	level := stepLevel(step)
	if err := lipcSet(puffin, "AvsVolume", level); err != nil {
		return fmt.Errorf("volume: step %d was refused: %w", step, err)
	}
	waitVolume(volumeSettle, func() bool { return volume.levelOK && volume.level == level })
	return nil
}

func SetMusicMute(on bool) error {
	state := 0
	if on {
		state = 1
	}
	if err := lipcSet(audiod, "Mute", state); err != nil {
		return fmt.Errorf("volume: the mute was refused: %w", err)
	}
	waitVolume(volumeSettle, func() bool { return volume.muteOK && volume.muted == on })
	return nil
}

func AwaitVolume(limit time.Duration) bool {
	return waitVolume(limit, func() bool { return volume.levelOK && volume.muteOK })
}

func waitVolume(limit time.Duration, done func() bool) bool {
	deadline := time.After(limit)
	for {
		volume.mu.Lock()
		if done() {
			volume.mu.Unlock()
			return true
		}
		changed := changedLocked()
		volume.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return false
		}
	}
}

func changedLocked() chan struct{} {
	if volume.changed == nil {
		volume.changed = make(chan struct{})
	}
	return volume.changed
}

func notifyLocked() {
	if volume.changed != nil {
		close(volume.changed)
		volume.changed = nil
	}
}

func FollowVolume() {
	said := false
	for {
		start := time.Now()
		err := followVolume()
		forgetVolume()
		if time.Since(start) >= volumeRetry {
			said = false
		}
		if !said {
			log.Printf("volume: %v; retrying every %v, and saying so once", err, volumeRetry)
			said = true
		}
		time.Sleep(volumeRetry)
	}
}

func followVolume() error {
	cmd := exec.Command(volumeWatchArgv[0], volumeWatchArgv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	stop := make(chan struct{})
	reread := make(chan struct{})
	go func() {
		defer close(reread)
		rereadVolume(stop)
	}()
	defer func() {
		close(stop)
		<-reread
	}()

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		volumeEvent(sc.Text())
	}
	scanErr := sc.Err()
	_ = cmd.Process.Kill()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return fmt.Errorf("%s ended: %v", volumeWatchArgv[0], waitErr)
}

func forgetVolume() {
	volume.mu.Lock()
	defer volume.mu.Unlock()
	volume.levelOK, volume.muteOK = false, false
	volume.levelGen++
	volume.muteGen++
	notifyLocked()
}

func rereadVolume(stop <-chan struct{}) {
	tick := time.NewTicker(volumeReread)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		readVolume()
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

func readVolume() {
	volume.mu.Lock()
	levelGen, muteGen := volume.levelGen, volume.muteGen
	volume.mu.Unlock()

	level, levelErr := lipcGet(audiod, "MainVolume")
	mute, muteErr := lipcGet(audiod, "Mute")

	volume.mu.Lock()
	defer volume.mu.Unlock()
	if levelErr == nil && volume.levelGen == levelGen {
		volume.level, volume.levelOK = clampLevel(level), true
	}
	if muteErr == nil && volume.muteGen == muteGen {
		volume.muted, volume.muteOK = mute != 0, true
	}
	notifyLocked()
}

func volumeEvent(line string) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return
	}
	volume.mu.Lock()
	defer volume.mu.Unlock()
	switch fields[0] {
	case "MainVolumeChanged":
		volume.level, volume.levelOK = clampLevel(n), true
		volume.levelGen++
	case "MuteChanged":
		volume.muted, volume.muteOK = n != 0, true
		volume.muteGen++
		if len(fields) > 2 {
			if level, err := strconv.Atoi(fields[2]); err == nil {
				volume.level, volume.levelOK = clampLevel(level), true
				volume.levelGen++
			}
		}
	default:
		return
	}
	notifyLocked()
}

func lipcGet(service, property string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lipcTimeout)
	defer cancel()
	out, err := lipcCommand(ctx, []string{lipcGetPath, "-i", "-q", service, property})
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func lipcSet(service, property string, value int) error {
	ctx, cancel := context.WithTimeout(context.Background(), lipcTimeout)
	defer cancel()
	_, err := lipcCommand(ctx, []string{lipcSetPath, "-i", "-q", service, property, strconv.Itoa(value)})
	return err
}
