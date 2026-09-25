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

func timingThenRate(t *testing.T, peer *wsPeer, server *serverSide) (lead, buffer, rate int) {
	t.Helper()
	lead, buffer = -1, -1
	for {
		kind, payload := nextJSON(t, peer, server)
		switch kind {
		case typeClientState:
			var s clientState
			if err := json.Unmarshal(payload, &s); err != nil {
				t.Fatalf("decoding %s: %v", typeClientState, err)
			}
			lead, buffer = s.Player.RequiredLeadTimeMS, s.Player.MinBufferMS
		case typeStreamRequestFormat:
			var r requestFormat
			if err := json.Unmarshal(payload, &r); err != nil {
				t.Fatalf("decoding %s: %v", typeStreamRequestFormat, err)
			}
			return lead, buffer, r.Player.SampleRate
		}
	}
}

func askedFor(t *testing.T, peer *wsPeer, server *serverSide) int {
	t.Helper()
	for {
		kind, payload := nextJSON(t, peer, server)
		if kind != typeStreamRequestFormat {
			continue
		}
		var r requestFormat
		if err := json.Unmarshal(payload, &r); err != nil {
			t.Fatalf("decoding %s: %v", typeStreamRequestFormat, err)
		}
		return r.Player.SampleRate
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
	if got := askedFor(t, peer, server); got != BluetoothRate {
		t.Errorf("the dot asked for %d Hz, want %d: a server sends the first format a"+
			" client offers, 48 kHz, until it is asked for another", got, BluetoothRate)
	}
}

func TestADotAsksAgainWhenItsOutputChanges(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	waitFor(t, "the output to be read 3 times", func() bool { return o.read() >= 3 })
	if sent := sentBeforeAPong(t, peer, server); slices.Contains(sent, typeStreamRequestFormat) {
		t.Errorf("a dot on its speaker asked for a format, and the server already sends"+
			" the first one it was offered: %v", sent)
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
	if slices.Contains(sent, typeStreamRequestFormat) {
		t.Errorf("a dot holding no player role asked for a format, which aiosendspin"+
			" flags as a payload for a role that is not active: %v", sent)
	}
	if slices.Contains(sent, typeClientState) {
		t.Errorf("a dot holding no player role declared a lead for its new output: %v", sent)
	}

	peer.writeBinary(server.sealJSON(t, typeServerActivate, serverActivate{
		Activities:  []string{activityPlayback},
		ActiveRoles: roles(rolePlayerV1),
	}))
	if got := askedFor(t, peer, server); got != BluetoothRate {
		t.Errorf("the player role taken again asked for %d Hz, want %d", got, BluetoothRate)
	}
}

func TestADotOnBluetoothDeclaresTheLongerLeadBeforeAskingForItsRate(t *testing.T) {
	ln := listenLocal(t)
	c, _ := outputClient(t, BluetoothRate)
	serveOn(t, c, ln)
	peer, server := activatedOnBluetooth(t, c, ln)
	lead, buffer, rate := timingThenRate(t, peer, server)
	if rate != BluetoothRate {
		t.Fatalf("the dot asked for %d Hz, want %d", rate, BluetoothRate)
	}
	if lead != 1100 {
		t.Errorf("before asking for %d Hz the dot declared a %d ms lead, want 1100:"+
			" the new stream is stamped from the lead the server holds when it opens,"+
			" and a Bluetooth output is about 430 ms deep", rate, lead)
	}
	if buffer != 900 {
		t.Errorf("before asking for %d Hz the dot declared a %d ms buffer, want 900:"+
			" a live stream is stamped from the buffer alone", rate, buffer)
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
	if lead, buffer, _ := timingThenRate(t, peer, server); lead != 1100 || buffer != 900 {
		t.Errorf("a speaker connecting declared a %d ms lead and a %d ms buffer,"+
			" want 1100 and 900", lead, buffer)
	}
	o.move(StreamRate)
	if lead, buffer, _ := timingThenRate(t, peer, server); lead != 350 || buffer != 500 {
		t.Errorf("a speaker going away declared a %d ms lead and a %d ms buffer, want 350"+
			" and 500: the longer figures make every stream start later, and a live one"+
			" play later throughout, and the whole group waits for them", lead, buffer)
	}
}

func TestADotDeclaresItsBluetoothBufferWhenOnlyTheBufferDiffers(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, StreamRate)
	c.BluetoothLeadMS = c.RequiredLeadMS
	serveOn(t, c, ln)
	peer, server, _ := bringUp(t, c, ln)

	o.move(BluetoothRate)
	if _, buffer, _ := timingThenRate(t, peer, server); buffer != 900 {
		t.Errorf("a speaker connecting declared a %d ms buffer, want 900: the buffer"+
			" follows the output even where the lead does not change", buffer)
	}
}

func TestADotDeclaresItsLeadOnceWhileItsOutputHolds(t *testing.T) {
	ln := listenLocal(t)
	c, o := outputClient(t, BluetoothRate)
	serveOn(t, c, ln)
	peer, server := activatedOnBluetooth(t, c, ln)
	timingThenRate(t, peer, server)

	polled := o.read()
	waitFor(t, "the output to be read 3 more times", func() bool { return o.read() >= polled+3 })
	if sent := sentBeforeAPong(t, peer, server); slices.Contains(sent, typeClientState) {
		t.Errorf("a dot whose output did not change declared its lead again: %v", sent)
	}
}
