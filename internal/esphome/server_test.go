package esphome

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bboe/overdub/internal/device"
	"github.com/bboe/overdub/internal/untrustedlog"
)

func TestSendDropsRatherThanBlockingOnAStalledClient(t *testing.T) {
	s := &Server{}
	c := &conn{out: make(chan frame, 2)}
	for i := 0; i < cap(c.out); i++ {
		if err := s.send(c, msgPingResponse, nil); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- s.send(c, msgPingResponse, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("send accepted a frame with no room; a stalled client would grow without bound")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked on a full queue")
	}
}

func TestTheReadLoopActsOnHandlesError(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgDisconnectRequest, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("the goodbye was not answered: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := c.recv()
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("the connection outlived the DisconnectRequest, so handle's error was ignored")
	}
}

func TestDisconnectIsAnsweredBeforeTheSocketCloses(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.send(msgDisconnectRequest, nil); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	msgType, _, err := c.recv()
	if err != nil {
		t.Fatalf("reading the reply to DisconnectRequest: %v", err)
	}
	if msgType != msgDisconnectResp {
		t.Errorf("replied with message %d, want %d", msgType, msgDisconnectResp)
	}
}

func serveOne(t *testing.T, s *Server) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		s.serveConn(server)
		close(done)
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serveConn was still running after its client closed")
		}
	})
	return client
}

func TestTheNinthConnectionIsRefused(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	for i := 0; i < maxConns; i++ {
		serveOne(t, s)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n == maxConns {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d connections were admitted", n, maxConns)
		}
		time.Sleep(5 * time.Millisecond)
	}

	over := serveOne(t, s)
	if err := over.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil &&
		!errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	_, err := over.Read(make([]byte, 1))
	if err == nil {
		t.Fatalf("connection %d was served; the cap does not hold", maxConns+1)
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Errorf("connection %d was admitted and left open; the cap does not hold",
			maxConns+1)
	}
}

func TestAnUnsubscribedClientGetsNoStates(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	s.uptime = func() (float32, bool) { return 1234, true }
	quiet := &conn{out: make(chan frame, sendQueue)}
	loud := &conn{out: make(chan frame, sendQueue), states: true}
	s.mu.Lock()
	s.conns[quiet] = struct{}{}
	s.conns[loud] = struct{}{}
	s.mu.Unlock()

	s.publish("sensors", s.readTicked())

	if n := len(quiet.out); n != 0 {
		t.Errorf("a client that never subscribed was sent %d frames", n)
	}
	if len(loud.out) == 0 {
		t.Error("a subscribed client was sent nothing")
	}
}

func TestAClientThatSaysNothingLosesItsSlot(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	s.handshakeWait = 200 * time.Millisecond
	silent := serveOne(t, s)
	if err := silent.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := silent.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("a silent client was sent something")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Error("a client that never spoke kept its slot past the handshake wait; " +
			"eight of those lock Home Assistant out")
	}
}

func TestAClientThatHasSentAFrameKeepsItsSlot(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	s.handshakeWait = 200 * time.Millisecond
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(3 * s.handshakeWait)

	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatalf("ping after going quiet: %v", err)
	}
	msgType, _, err := c.recv()
	if err != nil {
		t.Fatalf("ping after going quiet: %v", err)
	}
	if msgType != msgPingResponse {
		t.Fatalf("answered with %d, want %d", msgType, msgPingResponse)
	}
}

func TestAnUnfinishedHandshakeDoesNotBuyTheGrace(t *testing.T) {
	psk := testPSK(t)
	s := testServer(t, psk)
	s.handshakeWait = 200 * time.Millisecond
	client := serveOne(t, s)
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	w := bufio.NewWriter(client)
	if err := writeNoiseFrame(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := readNoiseFrame(bufio.NewReader(client), maxDataFrame); err != nil {
		t.Fatalf("the server hello did not arrive: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the connection was still open, and sent something")
		}
	case <-time.After(3 * time.Second):
		t.Error("a half-finished handshake kept its slot past the handshake wait")
	}
}

func TestALongClientNameIsCutBeforeItReachesTheLog(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	var p pb
	p.str(1, strings.Repeat("\xff", maxNoiseMessage-16))
	if err := c.send(msgHelloRequest, p.b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("hello was not answered: %v", err)
	}

	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("ping was not answered: %v", err)
	}

	if n := len(out.String()); n > 1024 {
		t.Errorf("one hello wrote %d bytes of log for a %d byte name", n, maxNoiseMessage-16)
	}
}

func TestOnlyThePollersReadTheDeviceAndNeverUnderTheLock(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)

	var mu sync.Mutex
	reads, underLock := 0, false
	read := make(chan struct{}, 1)
	watch := func(v float32) func() (float32, bool) {
		return func() (float32, bool) {
			mu.Lock()
			reads++
			locked := false
			for i := 0; i < 200; i++ {
				if s.mu.TryLock() {
					s.mu.Unlock()
					locked = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !locked {
				underLock = true
			}
			select {
			case read <- struct{}{}:
			default:
			}
			mu.Unlock()
			return v, true
		}
	}
	s.uptime, s.wifi = watch(1), watch(-48)
	s.volumes, s.cpu, s.memory = speakerReads(watch(40)), watch(41.3), watch(126.5)
	s.jack = func() (bool, bool) {
		occupied, _ := watch(1)()
		return occupied != 0, true
	}
	s.sound = func() (bool, bool) {
		playing, _ := watch(0)()
		return playing != 0, true
	}
	s.micMute = func() (bool, bool) {
		muted, _ := watch(0)()
		return muted != 0, true
	}
	s.alexa = func() (bool, bool) {
		registered, _ := watch(1)()
		return registered != 0, true
	}

	go s.Poll(MinSensorTick, time.Hour)

	for deadline := time.Now().Add(3 * time.Second); ; {
		s.mu.Lock()
		published := len(s.published)
		s.mu.Unlock()
		if published > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sensor poll never published, so this test would measure nothing")
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	polled := reads
	if underLock {
		t.Error("the poll read the device with the server lock held")
	}
	mu.Unlock()
	if polled == 0 {
		t.Fatal("the poll read nothing, so this test would pass on a server that never reads")
	}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < sensorCount; n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	const readersAWakeCosts = 7 // cpu, memory, volumes, jack, sound, mic, registration
	mu.Lock()
	defer mu.Unlock()
	if underLock {
		t.Error("the device was read with the server lock held")
	}
	if ceiling := 2*polled + readersAWakeCosts; reads > ceiling {
		t.Errorf("answering a subscriber took %d readings beyond the wake's own; the snapshot has to replay what was published, or the two readers can disagree",
			reads-ceiling)
	}
}

func TestAMalformedHelloIsNotAnswered(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	conn := &conn{sock: fakeAddr{}, out: make(chan frame, sendQueue)}
	if err := s.handle(conn, msgHelloRequest, []byte{0x08}); err == nil {
		t.Error("a truncated HelloRequest was accepted")
	}
	if len(conn.out) != 0 {
		t.Errorf("a truncated HelloRequest was answered with %d frames", len(conn.out))
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func restoreLog(t *testing.T, buf *lockedBuffer) func() {
	t.Helper()
	was := log.Writer()
	log.SetOutput(buf)
	return func() { log.SetOutput(was) }
}

func TestChurnCannotOutrunTheLogRateLimit(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", psk)

	for i := 0; i < 200; i++ {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() { s.serveConn(server); close(done) }()
		if i%2 == 1 {
			w := bufio.NewWriter(client)
			_ = writeNoiseFrame(w, nil)
			_ = w.Flush()
			_, _ = readNoiseFrame(bufio.NewReader(client), maxDataFrame)
		}
		client.Close()
		<-done
	}
	if lines := strings.Count(out.String(), "\n"); lines > 2*untrustedlog.Burst {
		t.Errorf("200 connect/disconnect cycles wrote %d log lines, want at most %d",
			lines, 2*untrustedlog.Burst)
	}

	before := out.String()
	if _, err := dial(t, s, psk); err != nil {
		t.Fatal(err)
	}
	added := strings.TrimPrefix(out.String(), before)
	if strings.Contains(added, "encrypted session established") {
		t.Errorf("an established session wrote past a spent burst: %s", added)
	}
}

type fakeAddr struct{ net.Conn }

func (fakeAddr) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1234} }

func TestAClientThatIsStillTalkingIsNeverPinged(t *testing.T) {
	if quiet := 2 * clientKeepalive; pingAfter <= quiet {
		t.Errorf("pingAfter is %v, but a client that is talking normally can be quiet for %v",
			pingAfter, quiet)
	}
}

func TestPollSensorsWillNotAcceptATickUnderTheFloor(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	done := make(chan struct{})
	go func() { s.PollSensors(time.Millisecond); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "raised to") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a 1ms tick was accepted; the log says %q", out.String())
}

func sensorReading(t *testing.T, msgType int, payload []byte) (uint32, float32, bool) {
	t.Helper()
	var key uint32
	var value float32
	missing := false
	seen := map[int]int{}
	if err := pbWalk(payload, func(f pbField) {
		seen[f.field] = f.wire
		switch f.field {
		case 1:
			key = uint32(f.num)
		case 2:
			switch msgType {
			case msgBinarySensorState, msgSwitchState:
				if f.num != 0 {
					value = 1
				}
			case msgSelectState, msgMediaPlayerState:
			default:
				value = math.Float32frombits(uint32(f.num))
			}
		case 3:
			if msgType == msgMediaPlayerState {
				value = math.Float32frombits(uint32(f.num))
				break
			}
			missing = f.num != 0
		}
	}); err != nil {
		t.Fatalf("state did not parse: %v", err)
	}
	if seen[1] != wireFixed32 {
		t.Errorf("the key went out as wire type %d, want fixed32 (%d)", seen[1], wireFixed32)
	}
	want := wireFixed32
	switch msgType {
	case msgBinarySensorState, msgSwitchState, msgMediaPlayerState:
		want = wireVarint
	case msgSelectState:
		want = wireBytes
	}
	if seen[2] != want {
		t.Errorf("the value went out as wire type %d, want %d for message %d", seen[2], want, msgType)
	}
	return key, value, missing
}

func speakerReads(read func() (float32, bool)) func() device.MusicVolume {
	return func() device.MusicVolume {
		v, ok := read()
		return device.MusicVolume{
			Max: 30, Speaker: v, SpeakerStep: int(v) * 30 / 100, SpeakerOK: ok,
			Jack: 70, JackStep: 21, JackOK: true,
			Bluetooth: 80, BluetoothStep: 24, BluetoothOK: true,
		}
	}
}

func stubSensors(s *Server) map[uint32]float32 {
	s.uptime = func() (float32, bool) { return 1234, true }
	s.wifi = func() (float32, bool) { return -48, true }
	s.cpu = func() (float32, bool) { return 41.3, true }
	s.memory = func() (float32, bool) { return 126.5, true }
	s.volumes = func() device.MusicVolume {
		return device.MusicVolume{
			Max: 30, Speaker: 40, SpeakerStep: 12, SpeakerOK: true,
			Jack: 70, JackStep: 21, JackOK: true,
			Bluetooth: 80, BluetoothStep: 24, BluetoothOK: true,
		}
	}
	s.jack = func() (bool, bool) { return true, true }
	s.sound = func() (bool, bool) { return false, true }
	s.micMute = func() (bool, bool) { return false, true }
	s.adbMode = func() (device.ADBMode, bool) { return device.ADBOff, true }
	s.alexa = func() (bool, bool) { return true, true }
	return map[uint32]float32{
		s.keyUptime: 1234, s.keyWifi: -48, s.keyVolume: 40,
		s.keyCPU: 41.3, s.keyMemory: 126.5, s.keyJack: 70, s.keyJackOn: 1,
		s.keySound: 0, s.keyMicMute: 0, s.keySpeaker: 0.7,
		s.button("action_button").keyMode: 0, s.button("mute_button").keyMode: 0,
		s.keyADB: 0, s.keyAlexa: 1, s.keyBT: 80,
	}
}

const sensorCount = 15

func listening(s *Server) *conn {
	c := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	return c
}

func quiet(s *Server, c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	for len(c.out) > 0 {
		<-c.out
	}
}

func pollAll(s *Server) []reading {
	c := listening(s)
	defer quiet(s, c)
	changed := s.publish("sensors", s.readTicked())
	changed = append(changed, s.publish("live", s.readLive())...)
	return append(changed, s.publish("live", []reading{s.readSound()})...)
}

func TestSubscribingGetsEverySensor(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)
	pollAll(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}

	for n := len(want); n > 0; n-- {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("a reading did not arrive: %v", err)
		}
		if msgType != msgSensorState && msgType != msgBinarySensorState &&
			msgType != msgSelectState && msgType != msgSwitchState &&
			msgType != msgMediaPlayerState {
			t.Fatalf("got message type %d, want a sensor (%d), binary sensor (%d), select (%d), "+
				"switch (%d) or media player (%d) state",
				msgType, msgSensorState, msgBinarySensorState, msgSelectState, msgSwitchState,
				msgMediaPlayerState)
		}
		key, value, missing := sensorReading(t, msgType, payload)
		expected, known := want[key]
		if !known {
			t.Fatalf("a reading arrived under key %d, which is no sensor of ours", key)
		}
		if value != expected {
			t.Errorf("key %d carried %v, want %v", key, value, expected)
		}
		if missing {
			t.Errorf("key %d was marked missing, so Home Assistant shows no value for a reading that succeeded", key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("%d sensor readings never arrived", len(want))
	}
}

func TestAReadingThatFailedIsSentAsMissing(t *testing.T) {
	for _, failing := range []string{"uptime", "wifi_signal", "volume", "jack_volume",
		"bluetooth_volume", "cpu_temperature", "memory_available", "audio_jack",
		"speaker_playing"} {
		t.Run(failing, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			psk := testPSK(t)
			s := testServer(t, psk)
			want := stubSensors(s)
			fail := func(f *func() (float32, bool)) { *f = func() (float32, bool) { return 0, false } }
			var failedKey uint32
			switch failing {
			case "uptime":
				fail(&s.uptime)
				failedKey = s.keyUptime
			case "wifi_signal":
				fail(&s.wifi)
				failedKey = s.keyWifi
			case "volume":
				s.volumes = func() device.MusicVolume {
					return device.MusicVolume{Max: 30, Jack: 70, JackStep: 21, JackOK: true,
						Bluetooth: 80, BluetoothStep: 24, BluetoothOK: true}
				}
				failedKey = s.keyVolume
			case "jack_volume":
				s.volumes = func() device.MusicVolume {
					return device.MusicVolume{Max: 30, Speaker: 40, SpeakerStep: 12, SpeakerOK: true,
						Bluetooth: 80, BluetoothStep: 24, BluetoothOK: true}
				}
				failedKey = s.keyJack
				delete(want, s.keySpeaker)
			case "bluetooth_volume":
				s.volumes = func() device.MusicVolume {
					return device.MusicVolume{Max: 30, Speaker: 40, SpeakerStep: 12, SpeakerOK: true,
						Jack: 70, JackStep: 21, JackOK: true}
				}
				failedKey = s.keyBT
			case "audio_jack":
				s.jack = func() (bool, bool) { return false, false }
				failedKey = s.keyJackOn
				delete(want, s.keySpeaker)
			case "speaker_playing":
				s.sound = func() (bool, bool) { return false, false }
				failedKey = s.keySound
			case "cpu_temperature":
				fail(&s.cpu)
				failedKey = s.keyCPU
			case "memory_available":
				fail(&s.memory)
				failedKey = s.keyMemory
			}

			pollAll(s)

			c, err := dial(t, s, psk)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.send(msgSubscribeStates, nil); err != nil {
				t.Fatal(err)
			}

			seen := false
			for n := 0; n < len(want); n++ {
				msgType, payload, err := c.recv()
				if err != nil {
					t.Fatalf("only %d of the %d readings arrived: %v", n, len(want), err)
				}
				key, value, missing := sensorReading(t, msgType, payload)
				if key != failedKey {
					if missing {
						t.Errorf("key %d was marked missing, and it was read successfully", key)
					}
					continue
				}
				seen = true
				if !missing {
					t.Errorf("a reading that failed went out as %v, which Home Assistant draws "+
						"as a measurement", value)
				}
			}
			if !seen {
				t.Errorf("the reading that failed was left out entirely; Home Assistant keeps " +
					"showing the last value it had")
			}
		})
	}
}

func TestTheMinuteTickCarriesOnlyItsOwnSensors(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	first := stubSensors(s)
	pollAll(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(first); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	s.uptime = func() (float32, bool) { return 5678, true }
	s.wifi = func() (float32, bool) { return -70, true }
	s.volumes = speakerReads(func() (float32, bool) { return 90, true })
	s.cpu = func() (float32, bool) { return 55.5, true }
	s.memory = func() (float32, bool) { return 64.5, true }
	s.publish("sensors", s.readTicked())

	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}

	want := map[uint32]float32{s.keyUptime: 5678, s.keyWifi: -70}
	live := map[uint32]string{
		s.keyVolume: "volume", s.keyCPU: "cpu temperature", s.keyMemory: "memory",
	}
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("a polled reading did not arrive: %v", err)
		}
		if msgType == msgPingResponse {
			break
		}
		key, value, _ := sensorReading(t, msgType, payload)
		if name, isLive := live[key]; isLive {
			t.Fatalf("the minute tick repeated the %s, which the short tick already sends when it changes", name)
		}
		expected, known := want[key]
		if !known {
			t.Fatalf("the poll sent key %d, which is no ticked sensor of ours", key)
		}
		if value != expected {
			t.Errorf("the poll sent %v for key %d, want %v", value, key, expected)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("the poll never sent %d of the readings that have no other source", len(want))
	}
}

func TestAQuietConnectionIsPingedRatherThanDropped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = 150 * time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}

	for round := 1; round <= 2; round++ {
		msgType, _, err := c.recv()
		if err != nil {
			t.Fatalf("round %d: no ping arrived: %v", round, err)
		}
		if msgType != msgPingRequest {
			t.Fatalf("round %d: got message type %d, want a ping (%d)", round, msgType, msgPingRequest)
		}
		if err := c.send(msgPingResponse, nil); err != nil {
			t.Fatalf("round %d: answering the ping: %v", round, err)
		}
	}
}

func TestAPeerThatWillNotAnswerThePingIsDropped(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = 150 * time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingRequest {
		t.Fatalf("no ping arrived: type %d, %v", msgType, err)
	}

	if _, _, err := c.recv(); err == nil {
		t.Error("a peer that never answered the ping was still connected")
	}
}

func TestTheKeepaliveBudgetMatchesESPHome(t *testing.T) {
	if pingAfter != 60*time.Second {
		t.Errorf("pingAfter is %v, want ESPHome's KEEPALIVE_TIMEOUT_MS of 60s", pingAfter)
	}
	if idleWait != 150*time.Second {
		t.Errorf("idleWait is %v, want ESPHome's KEEPALIVE_DISCONNECT_TIMEOUT of 150s", idleWait)
	}

	if live := NewServer("dot", "model", "", "00:00:5E:00:53:00", make([]byte, noisePSKLen)); live.pingWait != 60*time.Second {
		t.Errorf("NewServer starts a connection on %v, want %v", live.pingWait, pingAfter)
	}

	s := &Server{pingWait: pingAfter}
	if spent := s.readWait(false) + s.readWait(true); spent != idleWait {
		t.Errorf("the read loop spends %v before it gives up, want %v", spent, idleWait)
	}
	if s.readWait(true) <= s.readWait(false) {
		t.Error("the wait after the ping is not the longer one, so a slow answer costs the connection")
	}
}

func TestAPeerThatHasProvedNothingIsNotPinged(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.handshakeWait = 600 * time.Millisecond
	s.pingWait = 50 * time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	msgType, _, err := c.recv()
	took := time.Since(start)
	if err == nil {
		t.Errorf("a peer that sent nothing was answered with message type %d", msgType)
	}
	if msgType == msgPingRequest {
		t.Error("a peer that never decrypted a frame was pinged, which buys it a second deadline")
	}
	if took > 3*s.handshakeWait {
		t.Errorf("the connection lasted %v, well past the %v budget: the client's own deadline ended it, not the server",
			took, s.handshakeWait)
	}
}

func TestTheWaitAfterThePingIsSpentOnTheClient(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = time.Second

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}

	start := time.Now()
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingRequest {
		t.Fatalf("no ping arrived: type %d, %v", msgType, err)
	}
	if _, _, err := c.recv(); err == nil {
		t.Fatal("a peer that never answered the ping was still connected")
	}
	lived := time.Since(start)

	if low, high := s.pingWait*11/5, s.pingWait*14/5; lived < low || lived > high {
		t.Errorf("a quiet connection lasted %v, want about %v (between %v and %v): the two waits are not %v then half again",
			lived, s.pingWait*5/2, low, high, s.pingWait)
	}
}

func TestAFrameThatStoppedPartWayIsNotResumed(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent int // bytes of the frame the server is given before the stall
	}{
		{"part of the header", 2},
		{"the whole header and none of the payload", 3},
		{"the header and part of the payload", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			psk := testPSK(t)
			s := testServer(t, psk)
			s.pingWait = 150 * time.Millisecond

			c, err := dial(t, s, psk)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.send(msgPingRequest, nil); err != nil {
				t.Fatal(err)
			}
			if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
				t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
			}

			inner := make([]byte, 4)
			binary.BigEndian.PutUint16(inner[0:2], uint16(msgPingRequest))
			sealed, err := c.out.Encrypt(nil, nil, inner)
			if err != nil {
				t.Fatal(err)
			}
			frame := make([]byte, 3, 3+len(sealed))
			frame[0] = leadEncrypted
			binary.BigEndian.PutUint16(frame[1:3], uint16(len(sealed)))
			frame = append(frame, sealed...)
			if tc.sent >= len(frame) {
				t.Fatalf("the frame is only %d bytes, so %d of it is all of it", len(frame), tc.sent)
			}
			if _, err := c.conn.Write(frame[:tc.sent]); err != nil {
				t.Fatal(err)
			}

			msgType, _, err := c.recv()
			if err == nil {
				t.Fatalf("a half-read frame was answered with message type %d rather than dropped", msgType)
			}
			if msgType == msgPingRequest {
				t.Error("the server pinged part-way through a frame, so the rest of it becomes a header")
			}
			if said := out.String(); !strings.Contains(said, "mid-frame") {
				t.Errorf("the log does not say the stream was left mid-frame: %s", said)
			}
		})
	}
}

func TestOnlyAnExpiredReadDrawsAPing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.pingWait = 2 * time.Second

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if msgType, _, err := c.recv(); err != nil || msgType != msgPingResponse {
		t.Fatalf("the server did not answer a ping: type %d, %v", msgType, err)
	}

	if err := writeNoiseFrame(c.w, []byte("not sealed under the key at all")); err != nil {
		t.Fatal(err)
	}
	if err := c.w.Flush(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	msgType, _, err := c.recv()
	took := time.Since(start)
	if err == nil {
		t.Fatalf("a frame that failed to decrypt was answered with message type %d", msgType)
	}
	if msgType == msgPingRequest {
		t.Error("a peer that sent an undecryptable frame was pinged rather than dropped")
	}
	if took > s.pingWait/2 {
		t.Errorf("the drop took %v, long enough that a deadline ended it rather than the frame", took)
	}
}

func TestAReadingIsPublishedOnlyWhenItChanges(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	pollAll(s)
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the first readings did not arrive: %v", err)
		}
	}

	s.uptime = func() (float32, bool) { return 1234, true }
	s.volumes = speakerReads(func() (float32, bool) { return 40, true })
	s.cpu = func() (float32, bool) { return 41.3, true }
	reads := func(v float32, ok bool) { s.wifi = func() (float32, bool) { return v, ok } }
	signal := func() []reading { return pollAll(s) }

	reads(-55, true)
	if got := signal(); len(got) != 1 {
		t.Fatal("a changed signal was not published")
	}
	if got := signal(); len(got) != 0 {
		t.Error("a reading equal to the published one was sent again")
	}

	reads(-60, true)
	signal()

	for _, expect := range []float32{-55, -60} {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the push carrying %v did not arrive: %v", expect, err)
		}
		key, value, missing := sensorReading(t, msgType, payload)
		if key != s.keyWifi || value != expect || missing {
			t.Errorf("a push carried key %d value %v missing %v, want the signal (%d) at %v",
				key, value, missing, s.keyWifi, expect)
		}
	}

	reads(0, true)
	signal()
	if _, _, err := c.recv(); err != nil {
		t.Fatalf("the zero did not arrive: %v", err)
	}
	reads(0, false)
	signal()
	msgType, payload, err := c.recv()
	if err != nil {
		t.Fatalf("the missing reading did not arrive: %v", err)
	}
	if _, _, missing := sensorReading(t, msgType, payload); !missing {
		t.Error("a reading that could not be taken went out as a measurement")
	}
}

func TestAnUnreadableFirstReadingIsStillPublished(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	if got := s.publish("volume", []reading{{key: s.keyVolume, value: 0, ok: false}}); len(got) != 1 {
		t.Error("the first reading was not published, so a volume that cannot be read is never reported")
	}
	if got := s.publish("volume", []reading{{key: s.keyVolume, value: 0, ok: false}}); len(got) != 0 {
		t.Error("the same unreadable volume was published twice")
	}
}

func TestASubscriberIsNeverLeftHoldingAValueThePollWillNotCorrect(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	s.uptime = func() (float32, bool) { return 1234, true }

	pollAll(s)

	s.volumes = speakerReads(func() (float32, bool) { return 50, true })
	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	var told float32
	for n := 0; n < sensorCount; n++ {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
		if key, v, _ := sensorReading(t, msgType, payload); key == s.keyVolume {
			told = v
		}
	}

	s.volumes = speakerReads(func() (float32, bool) { return 40, true })
	if got := pollAll(s); len(got) != 0 {
		t.Fatal("the poll published; this test no longer covers the case it was written for")
	}

	if told != 40 {
		t.Errorf("the subscriber was told %v and the device reads 40, with no push left to correct it", told)
	}
}

type lockWatchingWriter struct {
	s      *Server
	mu     sync.Mutex
	held   bool
	writes int
}

func (w *lockWatchingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.s.mu.TryLock() {
		w.s.mu.Unlock()
	} else {
		w.held = true
	}
	return len(p), nil
}

func TestPublishLogsWhatItCouldNotSendAfterDroppingTheLock(t *testing.T) {
	s := testServer(t, testPSK(t))
	w := &lockWatchingWriter{s: s}
	was := log.Writer()
	log.SetOutput(w)
	defer log.SetOutput(was)

	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	stalled := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame), states: true}
	s.mu.Lock()
	s.conns[stalled] = struct{}{}
	s.mu.Unlock()

	s.publish("volume", []reading{{key: s.keyVolume, value: 40, ok: true}})

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writes == 0 {
		t.Fatal("nothing was logged, so this test would pass on a publish that never reports a failure")
	}
	if w.held {
		t.Error("a failure was logged with the server lock held")
	}
}

func TestAFailedSendDropsTheConnectionRatherThanContinuing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	stalled := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame, 1), states: true}
	s.mu.Lock()
	s.conns[stalled] = struct{}{}
	s.mu.Unlock()

	s.publish("sensors", []reading{
		{key: s.keyUptime, value: 1, ok: true},
		{key: s.keyWifi, value: -48, ok: true},
		{key: s.keyVolume, value: 40, ok: true},
	})

	s.mu.Lock()
	_, still := s.conns[stalled]
	s.mu.Unlock()
	if still {
		t.Error("a connection that could not take a whole push is still in the table")
	}
	if n := len(stalled.out); n != 1 {
		t.Errorf("the stalled connection was queued %d frames, so this is not the case the test means to cover", n)
	}
}

func TestTheLivePollSleepsUntilSomebodySubscribes(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)

	var mu sync.Mutex
	reads := 0
	s.volumes = speakerReads(func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	})

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}

	go s.PollLive(30 * time.Second)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	idle := reads
	mu.Unlock()
	if idle != 0 {
		t.Errorf("the poll read the device %d times for a connection that never subscribed", idle)
	}

	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting for the volume: %v", err)
		}
		if key, v, _ := sensorReading(t, msgType, payload); key == s.keyVolume {
			if v != 40 {
				t.Errorf("the woken poll published %v, want 40", v)
			}
			return
		}
	}
	t.Error("subscribing did not wake the volume poll, so the reading waits for a tick that is half a minute away")
}

func TestResubscribingDoesNotBuyAnotherReading(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	var mu sync.Mutex
	reads := 0
	count := func(v float32) func() (float32, bool) {
		return func() (float32, bool) { mu.Lock(); reads++; mu.Unlock(); return v, true }
	}
	s.uptime, s.wifi = count(1234), count(-48)
	s.volumes = speakerReads(count(40))

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	go s.PollLive(time.Hour)
	go s.PollSensors(time.Hour)

	ask := func(i int) {
		t.Helper()
		if err := c.send(msgSubscribeStates, nil); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		for n := 0; n < sensorCount; n++ {
			if _, _, err := c.recv(); err != nil {
				t.Fatalf("draining subscribe %d: %v", i, err)
			}
		}
	}

	ask(0)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	first := reads
	mu.Unlock()
	if first == 0 {
		t.Fatal("the first subscribe woke nothing, so this test would pass on a server that never reads")
	}

	const asks = 50
	for i := 1; i <= asks; i++ {
		ask(i)
	}
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if reads != first {
		t.Errorf("%d further subscribe requests drew %d more readings of the device; a peer holding the key can ask as fast as it likes",
			asks, reads-first)
	}
}

func TestSubscribingWakesTheSensorPoll(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	s.wakeGap = 10 * time.Millisecond
	want := stubSensors(s)

	var mu sync.Mutex
	uptime := want[s.keyUptime]
	read := make(chan struct{}, 1)
	s.uptime = func() (float32, bool) {
		mu.Lock()
		defer mu.Unlock()
		select {
		case read <- struct{}{}:
		default:
		}
		return uptime, true
	}
	pollAll(s)
	<-read

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	go s.PollSensors(time.Hour)
	<-read

	mu.Lock()
	uptime = 9999
	mu.Unlock()

	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("subscribing left the uptime at its published value with no read to correct it before the next tick: %v", err)
		}
		if key, v, _ := sensorReading(t, msgType, payload); key == s.keyUptime && v == 9999 {
			return
		}
	}
}

func TestTheLivePollKeepsReadingOnItsOwnTick(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	var mu sync.Mutex
	reads := 0
	s.volumes = speakerReads(func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	})

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	pollAll(s)
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	mu.Lock()
	before := reads
	mu.Unlock()

	go s.PollLive(20 * time.Millisecond)
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if reads-before < 3 {
		t.Errorf("the poll read %d times in 400ms on a 20ms tick; a poll that only wakes reads once and stops",
			reads-before)
	}
}

func TestTheExpensiveReadingsSkipMostTicks(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	var mu sync.Mutex
	reads := 0
	s.volumes = speakerReads(func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	})

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	pollAll(s)
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	mu.Lock()
	before := reads
	mu.Unlock()

	const tick = 10 * time.Millisecond
	start := time.Now()
	go s.PollLive(tick)
	time.Sleep(600 * time.Millisecond)
	ticks := int(time.Since(start) / tick)

	mu.Lock()
	defer mu.Unlock()
	got := reads - before
	if got == 0 {
		t.Fatal("the poll never read; the tick is not reaching the expensive readings at all")
	}
	if got > ticks/2 {
		t.Errorf("the expensive readings ran %d times in %d ticks, which is more than half of "+
			"them; HeavyEvery is %d, so about a third is what it should be",
			got, ticks, HeavyEvery)
	}
}

func TestSoundIsReadOftenerThanTheExpensiveReadings(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	var mu sync.Mutex
	sound, heavy := 0, 0
	s.sound = func() (bool, bool) {
		mu.Lock()
		sound++
		mu.Unlock()
		return false, true
	}
	s.volumes = speakerReads(func() (float32, bool) {
		mu.Lock()
		heavy++
		mu.Unlock()
		return 40, true
	})

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	pollAll(s)
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	mu.Lock()
	sound, heavy = 0, 0
	mu.Unlock()

	const tick = 10 * time.Millisecond
	start := time.Now()
	go s.PollLive(tick)
	time.Sleep(600 * time.Millisecond)
	ticks := int(time.Since(start) / tick)

	mu.Lock()
	defer mu.Unlock()
	if sound == 0 || heavy == 0 {
		t.Fatalf("sound read %d times and the expensive readings %d; neither should be zero",
			sound, heavy)
	}
	if sound <= heavy {
		t.Errorf("sound read %d times and the expensive readings %d in %d ticks; sound is on "+
			"every %d ticks and they are on every %d, so it should be the larger",
			sound, heavy, ticks, SoundEvery, HeavyEvery)
	}
	if ratio := HeavyEvery / SoundEvery; sound < heavy*ratio/2 {
		t.Errorf("sound read %d times and the expensive readings %d; sound is on every %d "+
			"ticks against their %d, so it should be about %d times as many",
			sound, heavy, SoundEvery, HeavyEvery, ratio)
	}
}

func TestEachStateArrivesAsTheMessageItsEntityWasListedUnder(t *testing.T) {
	for _, tt := range []struct {
		name  string
		sound func() (bool, bool)
	}{
		{"every reading taken", func() (bool, bool) { return true, true }},
		{"the speaker unreadable", func() (bool, bool) { return false, false }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			psk := testPSK(t)
			s := testServer(t, psk)
			want := stubSensors(s)
			elsewhere := map[uint32]struct {
				name    string
				msgType int
			}{
				s.keyJackOn:                       {"audio_jack", msgBinarySensorState},
				s.keySound:                        {"speaker_playing", msgBinarySensorState},
				s.keyMicMute:                      {"microphone_muted", msgSwitchState},
				s.keySpeaker:                      {"speaker", msgMediaPlayerState},
				s.button("action_button").keyMode: {"action_button_mode", msgSelectState},
				s.button("mute_button").keyMode:   {"mute_button_mode", msgSelectState},
				s.keyADB:                          {"network_adb", msgSelectState},
				s.keyAlexa:                        {"alexa_registered", msgBinarySensorState},
			}

			s.sound = tt.sound
			pollAll(s)
			c, err := dial(t, s, psk)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.send(msgSubscribeStates, nil); err != nil {
				t.Fatal(err)
			}

			seen := map[uint32]bool{}
			for n := 0; n < len(want); n++ {
				msgType, payload, err := c.recv()
				if err != nil {
					t.Fatalf("only %d of the %d states arrived: %v", n, len(want), err)
				}
				key, _, _ := sensorReading(t, msgType, payload)
				seen[key] = true
				if want, special := elsewhere[key]; special {
					if msgType != want.msgType {
						t.Errorf("%s went out as message %d, want %d: that is not the "+
							"message Home Assistant listed it under, and it will not read "+
							"the fields it finds", want.name, msgType, want.msgType)
					}
					continue
				}
				if msgType != msgSensorState {
					t.Errorf("the sensor with key %d went out as message %d, want "+
						"SensorStateResponse (%d)", key, msgType, msgSensorState)
				}
			}
			for key, want := range elsewhere {
				if !seen[key] {
					t.Errorf("%s never arrived at all", want.name)
				}
			}
		})
	}
}

func TestPollLiveArmsTheGapGuardAndStillReports(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	s.sound = func() (bool, bool) { return true, true }

	const tick = 20 * time.Millisecond
	s.onDelay, s.offDelay = 40*time.Millisecond, 40*time.Millisecond

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	go s.PollLive(tick)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		gap, on := s.soundGap, s.published[s.keySound]
		s.mu.Unlock()
		if gap == tick*SoundEvery*2 && on.value == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.mu.Lock()
	gap := s.soundGap
	s.mu.Unlock()
	if want := tick * SoundEvery * 2; gap != want {
		t.Errorf("PollLive left soundGap at %v, want %v (twice the sample interval)", gap, want)
	}
	t.Errorf("continuous sound was never reported: soundGap is %v against a sample every %v, "+
		"so every reading looks late and the on clock never accumulates", gap, tick*SoundEvery)
}

func TestTheSensorPollPublishesBeforeItsFirstTick(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)

	go s.PollSensors(time.Hour)
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.published) == 0 {
		t.Error("nothing was published before the first tick, so a subscriber arriving inside it is told nothing")
	}
}

func TestOnlyTheChangedReadingOfABatchIsSent(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < sensorCount; n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the snapshot did not arrive: %v", err)
		}
	}

	s.publish("sensors", []reading{
		{key: s.keyUptime, value: 4321, ok: true},
		{key: s.keyWifi, value: stubSensors(s)[s.keyWifi], ok: true},
	})
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}

	for {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting on the push: %v", err)
		}
		if msgType == msgPingResponse {
			return
		}
		key, value, _ := sensorReading(t, msgType, payload)
		if key != s.keyUptime {
			t.Errorf("a reading equal to the published one was sent again: key %d carried %v", key, value)
		}
	}
}

func TestASecondSubscriberCostsNoReading(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	var mu sync.Mutex
	reads := 0
	s.volumes = speakerReads(func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return reads }

	s.wakeGap = 10 * time.Millisecond
	go s.PollLive(time.Hour)
	time.Sleep(100 * time.Millisecond)

	subscribe := func(what string) *client {
		t.Helper()
		c, err := dial(t, s, psk)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := c.send(msgSubscribeStates, nil); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for n := 0; n < sensorCount; n++ {
			if _, _, err := c.recv(); err != nil {
				t.Fatalf("%s snapshot: %v", what, err)
			}
		}
		time.Sleep(120 * time.Millisecond)
		return c
	}

	before := count()
	subscribe("the first subscriber")
	first := count() - before
	if first == 0 {
		t.Fatal("the first subscriber woke nothing, so the published volume it was answered from could be any age")
	}

	after := count()
	for i := 0; i < 3; i++ {
		subscribe("a later subscriber")
	}
	if got := count() - after; got != 0 {
		t.Errorf("three connections arriving after one was already subscribed drew %d readings; the published state was already current",
			got)
	}
}

func TestWakingAgainInsideTheGapReadsNothing(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	pollAll(s)

	var mu sync.Mutex
	reads := 0
	s.volumes = speakerReads(func() (float32, bool) {
		mu.Lock()
		reads++
		mu.Unlock()
		return 40, true
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return reads }

	s.wakeGap = 2 * time.Second
	go s.PollLive(time.Hour)
	time.Sleep(100 * time.Millisecond)

	churn := func(round int) {
		c, err := dial(t, s, psk)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if err := c.send(msgSubscribeStates, nil); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		for n := 0; n < sensorCount; n++ {
			if _, _, err := c.recv(); err != nil {
				t.Fatalf("round %d snapshot: %v", round, err)
			}
		}
		time.Sleep(80 * time.Millisecond)
		c.conn.Close()
		for i := 0; i < 100; i++ {
			if !s.anyStateSubscriber() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("round %d: the connection was never dropped", round)
	}

	before := count()
	churn(1)
	first := count() - before
	if first == 0 {
		t.Fatal("the first subscriber read nothing, so this test would pass on a server that never reads")
	}
	churn(2)
	churn(3)

	if got := count() - before; got != first {
		t.Errorf("three idle-to-active cycles inside a %v gap drew %d readings, want the %d of the first",
			s.wakeGap, got, first)
	}
}

func TestEveryListedSensorHasAPollThatPublishesIt(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)

	want := map[uint32]bool{}
	for _, entity := range listedWithState(t, s) {
		want[uint32(entity[2].num)] = true
	}
	if len(want) == 0 {
		t.Fatal("nothing is listed, so this test would pass on a server that polls nothing")
	}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	s.Poll(MinSensorTick, 20*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for len(want) > 0 && time.Now().Before(deadline) {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting on the polls: %v", err)
		}
		key, _, _ := sensorReading(t, msgType, payload)
		delete(want, key)
	}
	for key := range want {
		t.Errorf("sensor %d is listed and no poll ever published it", key)
	}
}

func TestTheLivePollSurvivesATickThatIsNotPositive(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)

	go s.PollLive(0)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(out.String(), "raised to") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a tick of zero was accepted; the log says %q", out.String())
}

func TestAReadingThatIsZeroInEveryFieldIsStillPublished(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	if got := s.publish("sensors", []reading{{}}); len(got) != 1 {
		t.Error("a reading that is zero in every field was taken for one already published")
	}
	if got := s.publish("sensors", []reading{{}}); len(got) != 0 {
		t.Error("the same reading was published twice")
	}
}

func TestAWakeThatCannotBeSentIsDroppedRatherThanWaitedOn(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)

	s.sensorWake <- struct{}{}
	s.liveWake <- struct{}{}

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgPingRequest, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		msgType, _, err := c.recv()
		if err != nil {
			t.Fatalf("the handler never came back from a wake it could not send, and it holds the server lock: %v", err)
		}
		if msgType == msgPingResponse {
			return
		}
	}
}

func TestTheSensorCountMatchesTheListing(t *testing.T) {
	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	if got := len(listedWithState(t, s)); got != sensorCount {
		t.Errorf("the server lists %d entities with a state and sensorCount is %d", got, sensorCount)
	}
}

func TestTheJackIsPublishedWhenItChanges(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	want := stubSensors(s)

	occupied := true
	s.jack = func() (bool, bool) { return occupied, true }

	c, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	pollAll(s)
	for n := 0; n < len(want); n++ {
		if _, _, err := c.recv(); err != nil {
			t.Fatalf("the first readings did not arrive: %v", err)
		}
	}

	occupied = false
	if got := pollAll(s); len(got) != 2 {
		t.Fatalf("unplugging published %d readings, want 2: the jack itself, and the media "+
			"player whose volume is now the speaker's rather than the socket's", len(got))
	}
	if got := pollAll(s); len(got) != 0 {
		t.Error("a jack state equal to the published one was sent again")
	}

	if value, missing, ok := jackAmong(t, c, s.keyJackOn, 2); !ok {
		t.Error("the unplug never arrived")
	} else if value != 0 || missing {
		t.Errorf("the unplug carried value %v missing %v, want the jack at 0", value, missing)
	}

	occupied = true
	if got := pollAll(s); len(got) != 2 {
		t.Fatalf("plugging in published %d readings, want 2", len(got))
	}
	if value, _, ok := jackAmong(t, c, s.keyJackOn, 2); !ok {
		t.Error("the plug never arrived")
	} else if value != 1 {
		t.Errorf("the plug carried value %v, want the jack at 1", value)
	}
}

func jackAmong(t *testing.T, c *client, key uint32, n int) (float32, bool, bool) {
	t.Helper()
	for ; n > 0; n-- {
		msgType, payload, err := c.recv()
		if err != nil {
			t.Fatalf("waiting on the jack: %v", err)
		}
		got, value, missing := sensorReading(t, msgType, payload)
		if got != key {
			continue
		}
		if msgType != msgBinarySensorState {
			t.Fatalf("the jack went out as message %d, want a binary sensor state (%d)",
				msgType, msgBinarySensorState)
		}
		return value, missing, true
	}
	return 0, false, false
}

func TestAnUnreadableJackIsMissingRatherThanUnplugged(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.jack = func() (bool, bool) { return false, false }

	for _, r := range s.readLive() {
		if r.key != s.keyJackOn {
			continue
		}
		if r.ok {
			t.Error("a jack that could not be read was reported as a state")
		}
		if r.kind != kindBinary {
			t.Error("the jack went out as a sensor rather than a binary sensor")
		}
		return
	}
	t.Error("the live poll carries no jack reading at all")
}

func TestAReturningSubscriberIsNotToldTheSpeakerWasPlaying(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	psk := testPSK(t)
	s := testServer(t, psk)
	stubSensors(s)
	shortSoundDelays(s)
	s.sound = func() (bool, bool) { return true, true }

	go s.PollLive(5 * time.Millisecond)

	firstSound := func(c *client, want float32, what string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			c.conn.SetReadDeadline(time.Now().Add(time.Second))
			msgType, payload, err := c.recv()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if msgType != msgBinarySensorState {
				continue
			}
			key, v, _ := sensorReading(t, msgType, payload)
			if key != s.keySound {
				continue
			}
			if v != want {
				t.Fatalf("%s: the first speaker reading was %v, want %v", what, v, want)
			}
			return
		}
		t.Fatalf("%s: no speaker reading arrived at all", what)
	}

	gone, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := gone.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	firstSound(gone, 0, "the first subscriber")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the speaker was never reported as playing, so nothing was left to carry over")
		}
		gone.conn.SetReadDeadline(time.Now().Add(time.Second))
		msgType, payload, err := gone.recv()
		if err != nil {
			t.Fatalf("waiting for the speaker to turn on: %v", err)
		}
		if msgType != msgBinarySensorState {
			continue
		}
		if key, v, _ := sensorReading(t, msgType, payload); key == s.keySound && v == 1 {
			break
		}
	}

	gone.conn.Close()
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, held := s.published[s.keySound]
		s.mu.Unlock()
		if !held {
			break
		}
		time.Sleep(time.Millisecond)
	}

	back, err := dial(t, s, psk)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.send(msgSubscribeStates, nil); err != nil {
		t.Fatal(err)
	}
	firstSound(back, 0, "the subscriber that came back")
}

func TestTheServerReadsTheDeviceEachEntityNames(t *testing.T) {
	s := NewServer("kitchen", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	for _, tt := range []struct {
		name      string
		got, want any
	}{
		{"uptime", s.uptime, device.UptimeSeconds},
		{"wifi", s.wifi, device.WifiSignal},
		{"volumes", s.volumes, device.MusicVolumes},
		{"jack", s.jack, device.JackOccupied},
		{"sound", s.sound, device.SpeakerPlaying},
		{"micMute", s.micMute, device.MicMuted},
		{"cpu", s.cpu, device.CPUTemperature},
		{"memory", s.memory, device.AvailableMemory},
	} {
		got := reflect.ValueOf(tt.got).Pointer()
		want := reflect.ValueOf(tt.want).Pointer()
		if got != want {
			t.Errorf("%s is wired to %s, not to the reader of that name",
				tt.name, runtime.FuncForPC(got).Name())
		}
	}
}

func TestAnUnreadableMicIsNotPublishedAtAll(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.micMute = func() (bool, bool) { return false, false }

	for _, r := range s.readLive() {
		if r.key == s.keyMicMute {
			t.Errorf("a microphone that could not be read was published as %+v", r)
		}
	}

	s.micMute = func() (bool, bool) { return true, true }
	for _, r := range s.readLive() {
		if r.key != s.keyMicMute {
			continue
		}
		if r.kind != kindSwitch {
			t.Errorf("the microphone went out as kind %d, want a switch (%d)", r.kind, kindSwitch)
		}
		return
	}
	t.Error("a microphone that could be read was not published")
}

func TestTheMicMuteIsPublishedWhenItChanges(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	muted := false
	s.micMute = func() (bool, bool) { return muted, true }

	pollAll(s)
	if got := pollAll(s); len(got) != 0 {
		t.Errorf("a microphone state equal to the published one was sent again: %v", got)
	}

	muted = true
	got := pollAll(s)
	if len(got) != 1 {
		t.Fatalf("muting published %d readings, want 1", len(got))
	}
	if got[0].key != s.keyMicMute || got[0].value != 1 || !got[0].ok {
		t.Errorf("the mute carried %+v, want the microphone (%d) at 1", got[0], s.keyMicMute)
	}
}

func TestAnUnreadableRegistrationIsMissingRatherThanUnregistered(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	s.alexa = func() (bool, bool) { return false, false }
	defer quiet(s, listening(s))

	for _, r := range s.readTicked() {
		if r.key != s.keyAlexa {
			continue
		}
		if r.ok {
			t.Error("a registration that could not be read was reported as a state, which " +
				"reads as a Dot that is not registered")
		}
		if r.kind != kindBinary {
			t.Error("the registration went out as a sensor rather than a binary sensor")
		}
		return
	}
	t.Error("the minute poll carries no registration reading at all")
}

func TestTheRegistrationIsNotForkedWhileNobodyIsListening(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := testServer(t, testPSK(t))
	stubSensors(s)
	reads := 0
	s.alexa = func() (bool, bool) {
		reads++
		return true, true
	}

	for _, r := range s.readTicked() {
		if r.key == s.keyAlexa {
			t.Error("the registration was read with nobody subscribed, which is a fork a " +
				"minute on a Dot Home Assistant has never been told about")
		}
	}
	if reads != 0 {
		t.Errorf("the registration was read %d times with nobody subscribed", reads)
	}

	defer quiet(s, listening(s))
	found := false
	for _, r := range s.readTicked() {
		if r.key == s.keyAlexa {
			found = true
		}
	}
	if !found || reads != 1 {
		t.Errorf("a subscriber got %d registration readings and %v in the tick; want one of each",
			reads, found)
	}
}

func TestTheFirstSubscriberGetsNoRegistrationUntilThePollTakesOne(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	stubSensors(s)
	s.publish("sensors", s.readTicked())

	c := &conn{out: make(chan frame, sendQueue), sock: fakeAddr{}, states: true}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	err := s.sendSensorsAt(c, s.snapshot())
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	carries := func(what string) bool {
		t.Helper()
		found := false
		for len(c.out) > 0 {
			f := <-c.out
			key, _, _ := sensorReading(t, f.msgType, f.payload)
			if key == s.keyAlexa {
				if f.msgType != msgBinarySensorState {
					t.Errorf("the registration went out in %s as message %d, want %d",
						what, f.msgType, msgBinarySensorState)
				}
				found = true
			}
		}
		return found
	}

	if carries("the snapshot") {
		t.Error("the snapshot carried a registration nothing had read; published holds only " +
			"readings that were taken")
	}
	if s.publish("sensors", s.readTicked()); !carries("the woken poll") {
		t.Error("the poll that the subscriber's wake starts never sent the registration the " +
			"snapshot had nothing to say about")
	}
}

func TestWhatAPeerMadeUsNoteIsSpentFromItsBudget(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	c := &conn{sock: fakeAddr{}}

	c.noted = "esphome api: a peer changed something"
	s.noteFrom(c)
	if !strings.Contains(out.String(), "a peer changed something") {
		t.Fatalf("the note was never logged at all:\n%s", out.String())
	}
	if c.noted != "" {
		t.Error("the note was not cleared, so it would be logged again next message")
	}

	c.said = "a client name"
	s.noteFrom(c)
	if c.said != "" {
		t.Error("the hello was not cleared, so every later message re-logs it and one" +
			" connection spends the whole burst on the same line")
	}

	for i := 0; i < untrustedlog.Burst; i++ {
		s.untrustedLog.Printf("esphome api: line %d", i)
	}
	before := out.String()

	c.noted = "esphome api: this one is past the budget"
	c.said = "past the budget too"
	s.noteFrom(c)

	if strings.Contains(out.String()[len(before):], "past the budget") {
		t.Error("a note escaped the rate limit; every line a peer can cause has to spend from it")
	}
}

func TestAPeerSuppliedStringIsBoundedBeforeItIsNoted(t *testing.T) {
	long := strings.Repeat("z", 300)

	for _, c := range []struct {
		name string
		note func(t *testing.T, s *Server, conn *conn)
	}{
		{"a network adb choice", func(_ *testing.T, s *Server, conn *conn) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.setADBLocked(conn, long)
		}},
		{"a button mode choice", func(t *testing.T, s *Server, conn *conn) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, b := range s.buttons {
				s.setModeLocked(conn, b, long)
				return
			}
			t.Fatal("no button is offered, so this case asserts nothing")
		}},
		{"an alexa command", func(t *testing.T, s *Server, conn *conn) {
			s.UseCommand(func(string) error { return nil })
			if err := s.handle(conn, msgTextCommand, keyedText(s.keyText, long)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a url to play", func(_ *testing.T, s *Server, conn *conn) {
			s.UsePlay(func(string) error { return nil })
			s.mu.Lock()
			defer s.mu.Unlock()
			s.playLocked(conn, long, false)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out lockedBuffer
			defer restoreLog(t, &out)()

			s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
			s.UseButton("action_button", func() string { return "intercept" }, func(string) {})
			conn := &conn{sock: fakeAddr{}}

			c.note(t, s, conn)

			if conn.noted == "" {
				t.Fatal("nothing was noted, so this path no longer reports to the peer")
			}
			if strings.Contains(conn.noted, long) {
				t.Errorf("the note carries all %d bytes the peer supplied; a peer sets the"+
					" length of every line it can cause unless it is cut", len(long))
			}
		})
	}
}

func TestAPlaybackFailureSpendsThePeerBudget(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	s := NewServer("dot-test", "Echo Dot", "", "00:00:5E:00:53:2A", nil)
	for i := 0; i < untrustedlog.Burst; i++ {
		s.untrustedLog.Printf("esphome api: line %d", i)
	}
	before := out.String()

	s.NotePlaybackFailed("alexa said no, at length")

	if strings.Contains(out.String()[len(before):], "alexa said no") {
		t.Error("a playback failure escaped the rate limit; a peer asking for a clip that" +
			" cannot play is what causes it, so it spends the peer's budget")
	}
}

func TestAFailedPushSpendsThePeerBudget(t *testing.T) {
	stall := func(t *testing.T, s *Server) {
		t.Helper()
		near, far := net.Pipe()
		t.Cleanup(func() { near.Close(); far.Close() })
		stalled := &conn{sock: fakeAddr{Conn: near}, out: make(chan frame), states: true, services: true}
		s.mu.Lock()
		s.conns[stalled] = struct{}{}
		s.mu.Unlock()
	}

	for _, c := range []struct {
		name string
		push func(s *Server)
	}{
		{"a sensor publish", func(s *Server) {
			s.publish("volume", []reading{{key: s.keyVolume, value: 40, ok: true}})
		}},
		{"a button press", func(s *Server) {
			s.FirePress("action_button", EventPressEnd, 1, 0)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var reported lockedBuffer
			restore := restoreLog(t, &reported)
			s := testServer(t, testPSK(t))
			stall(t, s)
			c.push(s)
			restore()
			if !strings.Contains(reported.String(), "failed") {
				t.Fatalf("this path no longer reports a failed push, so the check below"+
					" would pass on a push that reports nothing:\n%s", reported.String())
			}

			var out lockedBuffer
			defer restoreLog(t, &out)()
			spent := testServer(t, testPSK(t))
			stall(t, spent)
			for i := 0; i < untrustedlog.Burst; i++ {
				spent.untrustedLog.Printf("esphome api: line %d", i)
			}
			before := len(out.String())

			c.push(spent)

			if len(out.String()) > before {
				t.Errorf("a failed push escaped the rate limit; a peer that holds a connection"+
					" open and stops reading is what causes these, so they spend its budget:\n%s",
					out.String()[before:])
			}
		})
	}
}
