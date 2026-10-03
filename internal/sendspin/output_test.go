package sendspin

import (
	"encoding/json"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeOutput struct {
	mu    sync.Mutex
	rate  int
	polls int
}

func (o *fakeOutput) at() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.polls++
	return o.rate
}

func (o *fakeOutput) move(rate int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rate = rate
}

func (o *fakeOutput) read() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.polls
}

func outputClient(t *testing.T, rate int) (*Client, *fakeOutput) {
	t.Helper()
	c, _ := playingClient(t)
	o := &fakeOutput{rate: rate}
	c.Config.OutputRate = o.at
	c.outputEvery = 10 * time.Millisecond
	c.RequiredLeadMS = 350
	c.BluetoothLeadMS = 1100
	c.MinBufferMS = 500
	c.BluetoothMinBufferMS = 900
	return c, o
}

func activatedOnBluetooth(t *testing.T, c *Client, ln net.Listener) (*wsPeer, *serverSide) {
	t.Helper()
	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	return peer, server
}

func nextState(t *testing.T, peer *wsPeer, server *serverSide) playerState {
	t.Helper()
	for {
		kind, payload := nextJSON(t, peer, server)
		if kind != typeClientState {
			continue
		}
		var s clientState
		if err := json.Unmarshal(payload, &s); err != nil {
			t.Fatalf("decoding %s: %v", typeClientState, err)
		}
		if s.Player == nil {
			t.Fatalf("%s carried no player object", typeClientState)
		}
		if s.Player.Format == nil {
			t.Fatalf("%s carried no format: a current server reads its absence as no"+
				" preference, and sends the first format the client offered", typeClientState)
		}
		return *s.Player
	}
}

func askedFor(t *testing.T, peer *wsPeer, server *serverSide) int {
	t.Helper()
	return nextState(t, peer, server).Format.SampleRate
}

func untilAsked(t *testing.T, peer *wsPeer, server *serverSide, rate int) playerState {
	t.Helper()
	for {
		if p := nextState(t, peer, server); p.Format.SampleRate == rate {
			return p
		}
	}
}

func sentBeforeAPong(t *testing.T, peer *wsPeer, server *serverSide) []string {
	t.Helper()
	if _, err := peer.conn.Write(frame(true, opPing, []byte("handled"))); err != nil {
		t.Fatalf("writing a ping: %v", err)
	}
	var kinds []string
	for {
		switch op, body := peer.read(); {
		case op == opPong && string(body) == "handled":
			return kinds
		case op == opBinary:
			kind, plain := server.open(t, body)
			if kind != msgJSON {
				continue
			}
			var env envelope
			if err := json.Unmarshal(plain, &env); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			kinds = append(kinds, env.Type)
		}
	}
}

func TestADotOnBluetoothAsksForItsOutputsRateOnceActivated(t *testing.T) {
	ln := listenLocal(t)
	c, _ := outputClient(t, BluetoothRate)
	serveOn(t, c, ln)
	peer := dialLocal(t, ln)
	server := driveServer(t, peer, serverPlan{
		clientPublic: c.Keys.Identity.Public,
		psk:          SentinelPSK(),
		cat:          categorySentinel,
	})
	peer.writeBinary(server.sealJSON(t, typeServerHello, serverHello{Name: "music assistant"}))
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	if p := untilAsked(t, peer, server, BluetoothRate); p.Format.Codec != codecFLAC {
		t.Errorf("the dot asked for %s at %d Hz, want %s: the codec it offers first",
			p.Format.Codec, BluetoothRate, codecFLAC)
	}
}

func TestADotAsksAgainWhenItsOutputChanges(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	waitFor(t, "the output to be read 3 times", func() bool { return o.read() >= 3 })
	if sent := sentBeforeAPong(t, peer, server); slices.Contains(sent, typeClientState) {
		t.Errorf("a dot whose output did not move stated its format again: %v", sent)
	}

	o.move(BluetoothRate)
	if got := askedFor(t, peer, server); got != BluetoothRate {
		t.Errorf("a speaker connecting asked for %d Hz, want %d", got, BluetoothRate)
	}
	o.move(StreamRate)
	if got := askedFor(t, peer, server); got != StreamRate {
		t.Errorf("a speaker going away asked for %d Hz, want %d: the dot's own speaker"+
			" runs at 48 kHz, and AudioFlinger resamples anything else", got, StreamRate)
	}
}

func TestARegainedPlayerRoleAsksForTheRateAgain(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	o.move(BluetoothRate)
	if got := askedFor(t, peer, server); got != BluetoothRate {
		t.Fatalf("a speaker connecting asked for %d Hz, want %d", got, BluetoothRate)
	}
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: &[]string{},
	}))
	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	if got := askedFor(t, peer, server); got != BluetoothRate {
		t.Errorf("a player role taken again asked for %d Hz, want %d: aiosendspin"+
			" builds the role afresh at the first format offered", got, BluetoothRate)
	}
}

func TestADotWithNoPlayerRoleAsksForNothing(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: &[]string{},
	}))
	handled(t, peer, server)
	o.move(BluetoothRate)
	polled := o.read()
	waitFor(t, "the output to be read 3 more times", func() bool { return o.read() >= polled+3 })
	sent := sentBeforeAPong(t, peer, server)
	if slices.Contains(sent, typeClientState) {
		t.Errorf("a dot holding no player role declared a format and lead for its new"+
			" output, which aiosendspin flags as a payload for a role that is not active:"+
			" %v", sent)
	}

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	untilAsked(t, peer, server, BluetoothRate)
}

func TestADotOnBluetoothDeclaresTheLongerLeadWithItsRate(t *testing.T) {
	ln := listenLocal(t)
	c, _ := outputClient(t, BluetoothRate)
	serveOn(t, c, ln)
	peer, server := activatedOnBluetooth(t, c, ln)
	p := untilAsked(t, peer, server, BluetoothRate)
	if p.RequiredLeadTimeMS != 1100 {
		t.Errorf("asking for %d Hz the dot declared a %d ms lead, want 1100: the new"+
			" stream is stamped from the lead the server holds when it opens, and a"+
			" Bluetooth output is about 430 ms deep", BluetoothRate, p.RequiredLeadTimeMS)
	}
	if p.MinBufferMS != 900 {
		t.Errorf("asking for %d Hz the dot declared a %d ms buffer, want 900: a live"+
			" stream is stamped from the buffer alone", BluetoothRate, p.MinBufferMS)
	}
}

func TestADotBackOnItsSpeakerDeclaresTheSpeakersLead(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	serveOn(t, c, ln)
	peer, server, state := bringUp(t, c, ln)
	if state.Player.RequiredLeadTimeMS != 350 || state.Player.MinBufferMS != 500 {
		t.Fatalf("a dot on its speaker declared a %d ms lead and a %d ms buffer,"+
			" want 350 and 500", state.Player.RequiredLeadTimeMS, state.Player.MinBufferMS)
	}

	o.move(BluetoothRate)
	if p := nextState(t, peer, server); p.RequiredLeadTimeMS != 1100 || p.MinBufferMS != 900 {
		t.Errorf("a speaker connecting declared a %d ms lead and a %d ms buffer,"+
			" want 1100 and 900", p.RequiredLeadTimeMS, p.MinBufferMS)
	}
	o.move(StreamRate)
	if p := nextState(t, peer, server); p.RequiredLeadTimeMS != 350 || p.MinBufferMS != 500 {
		t.Errorf("a speaker going away declared a %d ms lead and a %d ms buffer, want 350"+
			" and 500: the longer figures make every stream start later, and a live one"+
			" play later throughout, and the whole group waits for them",
			p.RequiredLeadTimeMS, p.MinBufferMS)
	}
}

func TestADotDeclaresItsBluetoothBufferWhenOnlyTheBufferDiffers(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	c.BluetoothLeadMS = c.RequiredLeadMS
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	o.move(BluetoothRate)
	if p := nextState(t, peer, server); p.MinBufferMS != 900 {
		t.Errorf("a speaker connecting declared a %d ms buffer, want 900: the buffer"+
			" follows the output even where the lead does not change", p.MinBufferMS)
	}
}

func TestADotDeclaresItsLeadOnceWhileItsOutputHolds(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, BluetoothRate)
	serveOn(t, c, ln)
	peer, server := activatedOnBluetooth(t, c, ln)
	untilAsked(t, peer, server, BluetoothRate)

	polled := o.read()
	waitFor(t, "the output to be read 3 more times", func() bool { return o.read() >= polled+3 })
	if sent := sentBeforeAPong(t, peer, server); slices.Contains(sent, typeClientState) {
		t.Errorf("a dot whose output did not change declared its lead again: %v", sent)
	}
}
