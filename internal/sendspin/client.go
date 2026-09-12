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

	MinBufferMS int

	handshakeAfter   time.Duration
	provisionalAfter time.Duration
	rolelessAfter    time.Duration
	pingEvery        time.Duration

	Peer *untrustedlog.Log

	mu    sync.Mutex
	held  *Session
	conns int
}

var (
	errBusy   = errors.New("another server already holds this client")
	errNoRoom = errors.New("too many connections already")
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
	c.mu.Unlock()
	c.Peer.Printf("sendspin listening on %s (client %q)", ln.Addr(), c.Config.Name)
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
		if err := c.enter(); err != nil {
			c.Peer.Printf("sendspin: %s refused: %d connections already",
				nc.RemoteAddr(), maxConns)
			nc.Close()
			continue
		}
		go func() {
			defer c.leave()
			c.serveConn(nc)
		}()
	}
}

func (c *Client) enter() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns >= maxConns {
		return errNoRoom
	}
	c.conns++
	return nil
}

func (c *Client) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns--
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
		if msg != msgJSON {
			once("sendspin: %q sent a %#x message, which is not handled yet", name, msg)
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
				if err := c.state(session); err != nil {
					return err
				}
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
		case typeStreamStart, typeStreamClear, typeStreamEnd, typeServerState, typeServerComm, typeServerTime:
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

func (c *Client) state(session *Session) error {
	return session.WriteJSON(typeClientState, clientState{
		Available: false,
		Player: &playerState{
			MinBufferMS:       c.MinBufferMS,
			SupportedCommands: []string{},
		},
	})
}
