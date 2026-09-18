package sendspin

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	typeClientTime = "client/time"

	answerWait = 5 * time.Second

	askFast   = 200 * time.Millisecond
	askBrisk  = 500 * time.Millisecond
	askSteady = time.Second
	askCalm   = 3 * time.Second

	briskAbove  = 5000
	steadyAbove = 2000
	calmAbove   = 1000

	stampCeiling = 1 << 50
)

var monotonicStart = time.Now()

func nowMicros() int64 { return time.Since(monotonicStart).Microseconds() }

type answer int

const (
	unasked answer = iota
	offClock
	backwards
	spent
	measured
)

func (a answer) String() string {
	switch a {
	case unasked:
		return "an answer to a question this client never asked"
	case offClock:
		return "stamps that are on no clock"
	case backwards:
		return "a reply it says it sent before the question reached it"
	case spent:
		return "the whole round trip claimed as its own"
	case measured:
		return "a measurement"
	}
	return "an outcome with no name"
}

type clientTime struct {
	ClientTransmitted int64 `json:"client_transmitted"`
}

type serverTime struct {
	ClientTransmitted int64 `json:"client_transmitted"`
	ServerReceived    int64 `json:"server_received"`
	ServerTransmitted int64 `json:"server_transmitted"`
}

type clock struct {
	filter  *timeFilter
	replied chan struct{}

	mu       sync.Mutex
	asked    int64
	awaiting bool
}

func newClock() *clock {
	return &clock{filter: newTimeFilter(), replied: make(chan struct{}, 1)}
}

func (k *clock) ask(s *Session) error {
	now := nowMicros()
	k.mu.Lock()
	k.asked, k.awaiting = now, true
	k.mu.Unlock()
	select {
	case <-k.replied:
	default:
	}
	return s.WriteJSON(typeClientTime, clientTime{ClientTransmitted: now})
}

func (k *clock) answered(stamp int64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.awaiting || stamp != k.asked {
		return false
	}
	k.awaiting = false
	return true
}

func (k *clock) observe(payload json.RawMessage, now int64) (answer, error) {
	var st serverTime
	if err := json.Unmarshal(payload, &st); err != nil {
		return unasked, fmt.Errorf("server/time: %w", err)
	}
	if !k.answered(st.ClientTransmitted) {
		return unasked, nil
	}
	select {
	case k.replied <- struct{}{}:
	default:
	}
	if !onAClock(st.ServerReceived) || !onAClock(st.ServerTransmitted) {
		return offClock, nil
	}
	if st.ServerTransmitted < st.ServerReceived {
		return backwards, nil
	}
	offset := ((st.ServerReceived - st.ClientTransmitted) + (st.ServerTransmitted - now)) / 2
	delay := ((now - st.ClientTransmitted) - (st.ServerTransmitted - st.ServerReceived)) / 2
	if delay <= 0 {
		return spent, nil
	}
	k.filter.Update(offset, delay, now)
	return measured, nil
}

func onAClock(us int64) bool { return us >= 0 && us <= stampCeiling }

func (k *clock) every() time.Duration {
	converged, spread := k.filter.state()
	switch {
	case !converged, spread >= briskAbove:
		return askFast
	case spread >= steadyAbove:
		return askBrisk
	case spread >= calmAbove:
		return askSteady
	default:
		return askCalm
	}
}

func pause(stop <-chan struct{}, woken <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-woken:
	case <-t.C:
	}
	return true
}

func (c *Client) keepTime(session *Session, nc net.Conn, stop <-chan struct{}) {
	k := session.clock
	for {
		if err := k.ask(session); err != nil {
			c.Peer.Printf("sendspin: this player could not ask its server for the time,"+
				" so the connection goes: %v", err)
			nc.Close()
			return
		}
		if !pause(stop, k.replied, waitOr(c.answerAfter, answerWait)) {
			return
		}
		if !pause(stop, nil, waitOr(c.timeEvery, k.every())) {
			return
		}
	}
}
