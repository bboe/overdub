package sendspin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bboe/overdub/internal/mdns"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	Service = "_sendspin._tcp.local."
	Port    = 8928
	Path    = "/sendspin"

	bufferSeconds  = 2
	BufferCapacity = StreamRate * StreamChannels * (StreamBitDepth / 8) * bufferSeconds

	maxConns = 8

	maxNoted = 16

	handshakeWait   = 30 * time.Second
	goodbyeWait     = 2 * time.Second
	provisionalWait = 30 * time.Second
	pingAfter       = 60 * time.Second
	idleWait        = pingAfter * 5 / 2

	typeClientState = "client/state"
	typeGroupUpdate = "group/update"
	typeServerState = "server/state"
	typeServerComm  = "server/command"
	typeStreamStart = "stream/start"
	typeStreamClear = "stream/clear"
	typeStreamEnd   = "stream/end"
	typeServerTime  = "server/time"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

var instance = bootToken(bootIDPath)

func bootToken(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := bytes.TrimSpace(raw)
	if len(id) == 0 {
		return ""
	}
	sum := sha256.Sum256(id)
	return hex.EncodeToString(sum[:4])
}

func Advert(name string) mdns.Advert {
	records := []string{"path=" + Path, "name=" + name}
	if instance != "" {
		records = append(records, "overdub_boot="+instance)
	}
	return mdns.Advert{
		Service: Service,
		Port:    Port,
		Records: records,
	}
}

type playerState struct {
	StaticDelayMS      int      `json:"static_delay_ms"`
	RequiredLeadTimeMS int      `json:"required_lead_time_ms"`
	MinBufferMS        int      `json:"min_buffer_ms"`
	SupportedCommands  []string `json:"supported_commands"`
}

type clientState struct {
	Available bool         `json:"available"`
	Player    *playerState `json:"player,omitempty"`
}

type groupUpdate struct {
	PlaybackState string `json:"playback_state"`
	GroupID       string `json:"group_id"`
	GroupName     string `json:"group_name"`
}

type Client struct {
	Config Config
	Keys   Keys
	PSKs   PSKSet

	MinBufferMS    int
	RequiredLeadMS int
	DelayMS        int
	DelayUnknown   bool
	SaveDelay      func(ms int) error

	Player Player

	delay     atomic.Int64
	keeping   atomic.Int64
	keepWake  chan struct{}
	keepEvery time.Duration

	handshakeAfter   time.Duration
	provisionalAfter time.Duration
	rolelessAfter    time.Duration
	pingEvery        time.Duration
	goodbyeAfter     time.Duration
	timeEvery        time.Duration
	reportEvery      time.Duration
	answerAfter      time.Duration

	Peer *untrustedlog.Log
	Play *untrustedlog.Log

	mu     sync.Mutex
	held   *Session
	live   map[net.Conn]*Session
	closed bool
}

var (
	errBusy   = errors.New("another server already holds this client")
	errNoRoom = errors.New("too many connections already")
	errClosed = errors.New("sendspin is switched off")
)

type noteSet struct {
	seen map[string]bool
}

func (n *noteSet) first(key string) bool {
	if n.seen[key] || len(n.seen) >= maxNoted {
		return false
	}
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	n.seen[key] = true
	return true
}

func waitOr(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}

func (c *Client) subject() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Peer == nil {
		return ""
	}
	return c.Peer.Subject
}

func (c *Client) Serve(ln net.Listener) error {
	defer ln.Close()
	c.mu.Lock()
	if c.Peer == nil {
		c.Peer = &untrustedlog.Log{Subject: "sendspin"}
	}
	if c.Play == nil {
		c.Play = &untrustedlog.Log{Subject: "sendspin playback"}
	}
	c.delay.Store(int64(max(0, min(c.DelayMS, maxStaticDelayMS))))
	c.keeping.Store(c.delay.Load())
	kept := make(chan struct{}, 1)
	c.keepWake = kept
	c.mu.Unlock()
	c.Peer.Printf("sendspin listening on %s (client %q)", ln.Addr(), c.Config.Name)
	if held := c.heldDelay(); held > 0 {
		c.Peer.Printf("sendspin: output delay of %s kept from last time", held)
	}
	if c.SaveDelay != nil {
		done, stopped := make(chan struct{}), make(chan struct{})
		defer func() {
			close(done)
			<-stopped
		}()
		go func() {
			defer close(stopped)
			c.keepDelays(kept, done)
		}()
	}
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			c.Peer.Printf("sendspin: accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if err := c.enter(nc); err != nil {
			c.Peer.Printf("sendspin: %s refused: %v", nc.RemoteAddr(), err)
			nc.Close()
			continue
		}
		go func() {
			defer c.leave(nc)
			c.serveConn(nc)
		}()
	}
}

func (c *Client) enter(nc net.Conn) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClosed
	}
	if len(c.live) >= maxConns {
		return errNoRoom
	}
	if c.live == nil {
		c.live = map[net.Conn]*Session{}
	}
	c.live[nc] = nil
	return nil
}

func (c *Client) track(nc net.Conn, session *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.live[nc]; ok {
		c.live[nc] = session
	}
}

func (c *Client) leave(nc net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.live, nc)
}

func (c *Client) conns() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.live)
}

func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	live := make(map[net.Conn]*Session, len(c.live))
	for nc, session := range c.live {
		live[nc] = session
	}
	c.mu.Unlock()

	budget := waitOr(c.goodbyeAfter, goodbyeWait)
	var saying sync.WaitGroup
	for nc, session := range live {
		saying.Add(1)
		go func() {
			defer saying.Done()
			if session != nil {
				session.ws.setIdle(budget)
				_ = nc.SetWriteDeadline(time.Now().Add(budget))
				said := make(chan struct{})
				defer close(said)
				go func() {
					select {
					case <-said:
					case <-time.After(2 * budget):
						nc.Close()
					}
				}()
				_ = session.Goodbye(goodbyeShutdown)
				if err := session.closeWS(closeNormal, goodbyeShutdown); err == nil {
					return
				}
			}
			nc.Close()
		}()
	}
	saying.Wait()
}

func (c *Client) serveConn(nc net.Conn) {
	defer nc.Close()
	if err := nc.SetDeadline(time.Now().Add(waitOr(c.handshakeAfter, handshakeWait))); err != nil {
		c.Peer.Printf("sendspin: deadline: %v", err)
		return
	}
	ws, err := Accept(nc, Path)
	if err != nil {
		c.Peer.Printf("sendspin: %v", err)
		return
	}
	session, err := Handshake(ws, c.Keys, c.PSKs)
	if err != nil {
		c.Peer.Printf("sendspin: %v", err)
		return
	}
	c.track(nc, session)
	name, err := session.Greet(c.Config)
	if err != nil {
		c.Peer.Printf("sendspin: %v", err)
		return
	}
	c.Peer.Printf("sendspin: handshake with %q complete on the %s psk",
		untrustedlog.Cut(name), session.Matched())

	if err := c.run(nc, ws, session, untrustedlog.Cut(name)); err != nil {
		c.Peer.Printf("sendspin: %q: %v", untrustedlog.Cut(name), err)
	}
	c.release(session)
}

func (c *Client) hold(s *Session) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held != nil && c.held != s {
		return errBusy
	}
	c.held = s
	return nil
}

func (c *Client) release(s *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == s {
		c.held = nil
	}
}

func (c *Client) run(nc net.Conn, ws *Conn, session *Session, name string) error {
	if err := nc.SetDeadline(time.Now().Add(waitOr(c.provisionalAfter, provisionalWait))); err != nil {
		return err
	}
	stated := false
	synced := false
	available := false
	var roleless *time.Timer
	var rolelessSince time.Time
	var rolelessSpent time.Duration
	defer func() {
		if roleless != nil {
			roleless.Stop()
		}
	}()
	stop := make(chan struct{})
	defer close(stop)

	var noted noteSet
	play := playback{player: c.Player, say: c.Play.Printf, name: name,
		delay: c.heldDelay()}
	defer play.stop()
	heard := chunkRun{every: waitOr(c.reportEvery, reportEvery), play: &play}
	defer func() { heard.done(c.Play, name) }()
	once := func(format string, args ...any) {
		if noted.first(format + "\x00" + fmt.Sprint(args...)) {
			c.Peer.Printf(format, args...)
		}
	}

	for {
		msg, body, err := session.Read()
		if err != nil {
			return err
		}
		heard.tick(c.Play, name)
		if msg != msgJSON {
			switch {
			case msg == binaryAudioChunk:
				chunk, err := session.AudioChunk(body)
				switch {
				case err != nil:
					once("sendspin: %q sent audio this player cannot read: %v", name, err)
				case chunk != nil:
					heard.took(c.Play, name, session, chunk)
					play.take(session, chunk)
				}
			case playerBinary(msg):
				once("sendspin: %q sent %#x, which the player reserves and does not carry"+
					" audio", name, msg)
			default:
				once("sendspin: %q sent a %#x message, which is not handled yet", name, msg)
			}
			continue
		}
		kind, payload, err := decodeEnvelope(body)
		if err != nil {
			return err
		}
		switch kind {
		case typeServerActivate:
			roles, err := session.Activate(payload)
			if err != nil {
				return err
			}
			if !holdsPlayer(roles) {
				heard.done(c.Play, name)
				play.stop()
			}
			if len(roles) == 0 {
				c.release(session)
				allowance := waitOr(c.rolelessAfter, provisionalWait)
				if rolelessSince.IsZero() {
					rolelessSince = time.Now()
				}
				left := allowance - rolelessSpent
				if left <= 0 {
					return fmt.Errorf("held no role for %s of this connection", allowance)
				}
				if roleless == nil {
					roleless = time.AfterFunc(left, func() {
						c.Peer.Printf("sendspin: %q held no role for %s, so its connection goes",
							name, allowance)
						nc.Close()
					})
				}
				once("sendspin: %q holds no role now", name)
				continue
			}
			if err := c.hold(session); err != nil {
				_ = session.Goodbye(goodbyeConcurrent)
				return err
			}
			if !rolelessSince.IsZero() {
				rolelessSpent += time.Since(rolelessSince)
				rolelessSince = time.Time{}
			}
			if roleless != nil {
				roleless.Stop()
				roleless = nil
			}
			if !stated {
				ws.setIdle(idleWait)
				go keepalive(ws, stop, waitOr(c.pingEvery, pingAfter))
				if err := c.state(session, available, play.delay); err != nil {
					return err
				}
				go c.keepTime(session, stop)
				stated = true
			}
			once("sendspin: %q activated %s", name, strings.Join(roles, ","))
		case typeGroupUpdate:
			var g groupUpdate
			if err := json.Unmarshal(payload, &g); err != nil {
				return fmt.Errorf("group/update: %w", err)
			}
			once("sendspin: group %q is %q", untrustedlog.Cut(g.GroupName),
				untrustedlog.Cut(g.PlaybackState))
		case typeServerTime:
			got, err := session.clock.observe(payload, nowMicros())
			if err != nil {
				return err
			}
			if got != measured {
				once("sendspin: %q answered the time with %s", name, got)
				continue
			}
			if !synced {
				if converged, spread := session.clock.filter.state(); converged {
					synced = true
					available = c.Player != nil
					c.Peer.Printf("sendspin: clock agreed with %q to within %d us", name, spread)
					if err := c.state(session, available, play.delay); err != nil {
						return err
					}
				}
			}
		case typeStreamStart:
			offered, err := session.StartStream(payload)
			if err != nil {
				return err
			}
			if offered != nil {
				heard.done(c.Play, name)
			}
			switch {
			case offered == nil:
				c.Play.Printf("sendspin: %q started a stream carrying nothing for a player",
					name)
			case session.Streaming():
				play.open()
				c.Play.Printf("sendspin: %q started a %s stream", name, offered)
			case !holdsPlayer(session.roles):
				c.Play.Printf("sendspin: %q started a stream for a role this client does"+
					" not hold", name)
			default:
				play.stop()
				c.Play.Printf("sendspin: %q offered a %s stream, and this player takes %s"+
					" %d Hz %d ch %d bit", name, offered, codecPCM, StreamRate,
					StreamChannels, StreamBitDepth)
			}
		case typeStreamEnd:
			ours, err := session.EndStream(payload)
			if err != nil {
				return err
			}
			if ours {
				heard.done(c.Play, name)
				play.finish()
				c.Play.Printf("sendspin: %q ended its stream", name)
			}
		case typeStreamClear:
			ours, err := session.ClearStream(payload)
			if err != nil {
				return err
			}
			if ours {
				heard.report(c.Play, name)
				play.clear()
				c.Play.Printf("sendspin: %q cleared what it had sent", name)
			}
		case typeServerComm:
			want, asked, ours, err := session.StaticDelay(payload)
			if err != nil {
				once("sendspin: %q sent a command this player will not take: %v", name, err)
				continue
			}
			if !ours {
				once("sendspin: %q sent a command that is not handled yet", name)
				continue
			}
			if asked != int(want/time.Millisecond) {
				once("sendspin: %q asked for a %d ms output delay, and the spec holds one"+
					" to 0 through %d, so %s is what this player takes", name, asked,
					maxStaticDelayMS, want)
			}
			if want == play.delay {
				continue
			}
			play.delay = want
			c.delay.Store(int64(want / time.Millisecond))
			c.keep(int(want / time.Millisecond))
			once("sendspin: %q is setting this player's output delay, and every summary"+
				" below says what it currently is", name)
			if err := c.state(session, available, want); err != nil {
				return err
			}
		case typeServerState:
			once("sendspin: %q is not handled yet", untrustedlog.Cut(kind))
		default:
			once("sendspin: ignoring %q", untrustedlog.Cut(kind))
		}
	}
}

func keepalive(ws *Conn, stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := ws.Ping(); err != nil {
				return
			}
		}
	}
}

func (c *Client) heldDelay() time.Duration {
	return time.Duration(c.delay.Load()) * time.Millisecond
}

func (c *Client) keep(ms int) {
	c.keeping.Store(int64(ms))
	c.mu.Lock()
	wake := c.keepWake
	c.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (c *Client) keepDelays(wake <-chan struct{}, done <-chan struct{}) {
	apart := waitOr(c.keepEvery, keepApart)
	written := max(0, min(c.DelayMS, maxStaticDelayMS))
	unknown, attempted, tries := c.DelayUnknown, written, 0
	settled := func() bool {
		ms := int(c.keeping.Load())
		if !unknown && ms == written {
			return true
		}
		if ms != attempted {
			attempted, tries = ms, 0
		}
		if tries >= keepTries {
			return true
		}
		if err := c.SaveDelay(ms); err != nil {
			c.Peer.Printf("sendspin: this dot took a %d ms output delay and could not"+
				" remember it: %v", ms, err)
			tries++
			return tries >= keepTries
		}
		written, unknown, tries = ms, false, 0
		return true
	}
	defer settled()
	pending := false
	if unknown {
		pending = !settled()
	}
	for {
		if !pending {
			select {
			case <-done:
				return
			case <-wake:
			}
		}
		select {
		case <-done:
			return
		case <-time.After(apart):
		}
		pending = !settled()
	}
}

func (c *Client) state(session *Session, available bool, delay time.Duration) error {
	return session.WriteJSON(typeClientState, clientState{
		Available: available,
		Player: &playerState{
			StaticDelayMS:      int(delay / time.Millisecond),
			RequiredLeadTimeMS: c.RequiredLeadMS,
			MinBufferMS:        c.MinBufferMS,
			SupportedCommands:  []string{commandStaticDelay},
		},
	})
}
