package esphome

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"log"
)

const (
	mdnsPort   = 5353
	dnsTypeA   = 1
	dnsTypePTR = 12
	dnsTypeTXT = 16
	dnsTypeSRV = 33
	dnsTypeANY = 255

	dnsClassIN         = 1
	dnsCacheFlush      = 0x8000
	dnsUnicastResponse = 0x8000 // in a query's class field

	ttlShared = 4500
	ttlHost   = 120

	goodbyeGap = time.Second

	addressPoll = 30 * time.Second

	// RFC 6762 section 6: a record must not go out on an interface more often
	// than this. It is also the only unbounded thing a peer can make this daemon
	// do, and every other peer-driven path here is capped: a 40-byte query draws
	// an answer of 328 bytes at the shortest name and 700 at the longest -name
	// permits, aimed at every host on the segment.
	multicastEvery = time.Second

	// A unicast reply reaches only the address the querier gave, which is also
	// why it is worth bounding: a spoofed source makes this daemon a reflector
	// pointed at somebody else, measured at eight to seventeen times
	// amplification over the query that triggers it.
	// Discovery needs a handful a second and never a flood.
	unicastBurst  = 20
	unicastWindow = time.Second

	esphomeService = "_esphomelib._tcp.local."
	dnssdMeta      = "_services._dns-sd._udp.local."
)

type Responder struct {
	Instance string
	MAC      string
	Iface    string
	Port     uint16

	mu         sync.Mutex
	conn       *net.UDPConn
	ip         net.IP
	subnet     *net.IPNet
	restart    bool
	lastSent   time.Time
	unicastEnd time.Time
	unicasts   int
	sendFailed error
	gone       bool
}

func (m *Responder) address() net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ip
}

// Kept rather than logged where it happens. A reply goes out because something
// on the network asked, so a line there is one an unauthenticated peer can
// repeat; docs/pitfalls.md gives the size of that hazard. The next address poll
// reports it instead, at most once per rebuild.
// Unicast replies are not throttled: they go to the host that asked, so they
// are neither the flood the RFC is about nor a way to reach anyone else.
func (m *Responder) mayMulticast() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.lastSent.IsZero() && time.Since(m.lastSent) < multicastEvery {
		return false
	}
	m.lastSent = time.Now()
	return true
}

func (m *Responder) mayUnicast() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now := time.Now(); now.After(m.unicastEnd) {
		m.unicastEnd = now.Add(unicastWindow)
		m.unicasts = 0
	}
	if m.unicasts >= unicastBurst {
		return false
	}
	m.unicasts++
	return true
}

// Only a failure to reach the group is this socket's problem. A unicast reply
// goes where the querier said, and a querier can name somewhere unroutable --
// port zero does it -- so counting that as our own failure hands anyone on the
// subnet a rebuild every poll, each one writing a line to the log.
func (m *Responder) noteReplyFailure(dst, group *net.UDPAddr, err error) {
	if dst.IP.Equal(group.IP) && dst.Port == group.Port {
		m.noteSendFailure(err)
	}
}

func (m *Responder) noteSendFailure(err error) {
	m.mu.Lock()
	if m.sendFailed == nil {
		m.sendFailed = err
	}
	m.mu.Unlock()
}

func (m *Responder) stale(ip net.IP) (reason string, rebuild bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	failed := m.sendFailed
	m.sendFailed = nil
	switch {
	case !m.ip.Equal(ip):
		return fmt.Sprintf("%s changed to %s", m.Iface, ip), true
	case failed != nil:
		return fmt.Sprintf("a send failed on %s (%v)", ip, failed), true
	}
	return "", false
}

func (m *Responder) onLink(ip net.IP) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subnet != nil && m.subnet.Contains(ip)
}

func (m *Responder) serviceName() string { return m.Instance + "." + esphomeService }
func (m *Responder) hostName() string    { return m.Instance + ".local." }

func (m *Responder) Run() {
	waiting, failing := false, false
	for {
		m.mu.Lock()
		withdrawn := m.gone
		m.mu.Unlock()
		if withdrawn {
			return
		}

		ip, subnet, err := interfaceIPv4(m.Iface)
		if err != nil {
			if !waiting {
				log.Printf("mdns: waiting for an address on %s (%v)", m.Iface, err)
				waiting = true
			}
			time.Sleep(5 * time.Second)
			continue
		}
		if waiting {
			log.Printf("mdns: %s came up as %s", m.Iface, ip)
			waiting = false
		}

		m.mu.Lock()
		m.ip, m.subnet = ip, subnet
		m.mu.Unlock()

		if err := m.serve(); err != nil {
			// Said once per run of failures, for the reason the address wait
			// above is: Run never returns, so nothing truncates this log, and a
			// line every thirty seconds for the life of the boot buries it.
			if !failing {
				log.Printf("mdns: %v; retrying every 30s", err)
				failing = true
			}
			time.Sleep(30 * time.Second)
			continue
		}
		failing = false
	}
}

func (m *Responder) open() (*net.UDPConn, error) {
	listenConfig := net.ListenConfig{
		Control: func(_, _ string, rawConn syscall.RawConn) error {
			var sockoptErr error
			err := rawConn.Control(func(fd uintptr) {
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
					sockoptErr = fmt.Errorf("SO_REUSEADDR: %w", err)
					return
				}
				const soReusePort = 0xf
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); err != nil {
					sockoptErr = fmt.Errorf("SO_REUSEPORT: %w", err)
				}
			})
			if err != nil {
				return err
			}
			return sockoptErr
		},
	}
	ip := m.address()
	packetConn, err := listenConfig.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", mdnsPort))
	if err != nil {
		return nil, fmt.Errorf("bind udp/%d: %w", mdnsPort, err)
	}
	conn, ok := packetConn.(*net.UDPConn)
	if !ok {
		packetConn.Close()
		return nil, fmt.Errorf("unexpected socket type %T", packetConn)
	}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		conn.Close()
		return nil, err
	}
	var sockoptErr error
	if err := rawConn.Control(func(fd uintptr) {
		var mreq syscall.IPMreq
		copy(mreq.Multiaddr[:], net.IPv4(224, 0, 0, 251).To4())
		copy(mreq.Interface[:], ip.To4())
		if err := syscall.SetsockoptIPMreq(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, &mreq); err != nil {
			sockoptErr = fmt.Errorf("IP_ADD_MEMBERSHIP: %w", err)
			return
		}
		var addr [4]byte
		copy(addr[:], ip.To4())
		if err := syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, addr); err != nil {
			sockoptErr = fmt.Errorf("IP_MULTICAST_IF: %w", err)
			return
		}
		syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 255)
		syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, 1)
	}); err != nil {
		conn.Close()
		return nil, err
	}
	if sockoptErr != nil {
		conn.Close()
		return nil, sockoptErr
	}
	return conn, nil
}

func (m *Responder) serve() error {
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

	m.beginCycle()
	conn, err := m.open()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.conn = conn
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.conn = nil
		m.mu.Unlock()
		conn.Close()
	}()
	conn.SetReadBuffer(65536)

	done := make(chan struct{})
	defer close(done)

	ip := m.address()
	log.Printf("mdns: announcing %s at %s:%d", m.serviceName(), ip, m.Port)
	go func() {
		if err := m.announce(conn, group); err != nil {
			select {
			case <-done:
				// The socket was closed under it on purpose; not a failure.
			default:
				log.Printf("mdns: announce failed: %v", err)
				m.noteSendFailure(err)
			}
		}
	}()

	go m.watchAddress(conn, done)

	buffer := make([]byte, 9000)
	for {
		n, src, err := conn.ReadFromUDP(buffer)
		if err != nil {
			m.mu.Lock()
			deliberate := m.restart
			m.restart = false
			m.mu.Unlock()
			if deliberate {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		dst, answer := m.replyTo(src, group, buffer[:n])
		if !answer {
			continue
		}
		if _, err := conn.WriteToUDP(m.records(ttlShared), dst); err != nil {
			m.noteReplyFailure(dst, group, err)
		}
	}
}

// Cleared for the new socket: both flags belong to the cycle that set them, and
// carried over they tear down a replacement that is working, reading as a
// deliberate rebuild so nothing says why.
func (m *Responder) beginCycle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restart, m.sendFailed = false, nil
}

// Every reason not to answer, in one place a test can reach: serve() holds a
// socket, so a decision left inside its loop is one nothing exercises.
func (m *Responder) replyTo(src, group *net.UDPAddr, packet []byte) (*net.UDPAddr, bool) {
	if !m.onLink(src.IP) {
		return nil, false
	}
	unicast, wanted := m.wants(packet)
	if !wanted {
		return nil, false
	}
	// A query already in the socket buffer when the goodbye went out would
	// otherwise be answered with live TTLs, re-advertising what was withdrawn.
	m.mu.Lock()
	withdrawn := m.gone
	m.mu.Unlock()
	if withdrawn {
		return nil, false
	}
	if unicast {
		if !m.mayUnicast() {
			return nil, false
		}
		return src, true
	}
	if !m.mayMulticast() {
		return nil, false
	}
	return group, true
}

func (m *Responder) announce(conn *net.UDPConn, group *net.UDPAddr) error {
	for i := 0; i < 2; i++ {
		payload := m.records(ttlShared)

		m.mu.Lock()
		gone := m.gone
		m.lastSent = time.Now()
		m.mu.Unlock()
		if gone {
			return nil
		}
		if _, err := conn.WriteToUDP(payload, group); err != nil {
			return err
		}
		time.Sleep(time.Second)
	}
	return nil
}

func (m *Responder) watchAddress(conn *net.UDPConn, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(addressPoll):
		}

		ip, subnet, err := interfaceIPv4(m.Iface)
		if err != nil {
			continue
		}
		reason, rebuild := m.stale(ip)
		if !rebuild {
			continue
		}

		// Re-checked after the poll, which is a netlink round trip: serve() can
		// have returned for its own reason while it ran, and a flag written into
		// a dead cycle is read by the next one, where it turns a real failure
		// into a silent deliberate rebuild.
		select {
		case <-done:
			return
		default:
		}

		m.mu.Lock()
		m.ip, m.subnet = ip, subnet
		m.restart = true
		m.mu.Unlock()
		log.Printf("mdns: %s, rebuilding the responder", reason)

		conn.Close()
		return
	}
}

func (m *Responder) wants(packet []byte) (unicast bool, wanted bool) {
	var parser dnsmessage.Parser
	header, err := parser.Start(packet)
	if err != nil || header.Response {
		return false, false
	}

	// Every question, not just up to the match: the answer section begins after
	// all of them, and the known-answer check below reads from there.
	matched := false
	for {
		question, err := parser.Question()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return false, false
		}
		if matched {
			continue
		}
		name := strings.ToLower(question.Name.String())
		qtype := uint16(question.Type)
		if (name == esphomeService && (qtype == dnsTypePTR || qtype == dnsTypeANY)) ||
			(name == strings.ToLower(m.serviceName()) && (qtype == dnsTypeSRV || qtype == dnsTypeTXT || qtype == dnsTypeANY)) ||
			(name == strings.ToLower(m.hostName()) && (qtype == dnsTypeA || qtype == dnsTypeANY)) {
			matched, unicast = true, uint16(question.Class)&dnsUnicastResponse != 0
		}
	}
	if !matched || m.alreadyKnown(&parser) {
		return false, false
	}
	return unicast, true
}

// RFC 6762 section 7.1: a query carrying our own PTR in its answer section, at
// half its TTL or better, is one that must not be answered. python-zeroconf's
// browser puts it there on every repeat, so without this every refresh from
// every Home Assistant on the segment draws a full multicast reply.
func (m *Responder) alreadyKnown(parser *dnsmessage.Parser) bool {
	for {
		answer, err := parser.AnswerHeader()
		if err != nil {
			return false
		}
		if answer.Type != dnsmessage.TypePTR || !strings.EqualFold(answer.Name.String(), esphomeService) {
			if parser.SkipAnswer() != nil {
				return false
			}
			continue
		}
		ptr, err := parser.PTRResource()
		if err != nil {
			return false
		}
		if strings.EqualFold(ptr.PTR.String(), m.serviceName()) && answer.TTL >= ttlShared/2 {
			return true
		}
	}
}

func (m *Responder) records(ttl uint32) []byte {
	hostTTL := ttl
	if hostTTL > ttlHost {
		hostTTL = ttlHost
	}
	// Shared: every ESPHome device on the segment answers this name, so the
	// cache-flush bit would expire all of theirs on each of our announcements.
	// The other three are ours alone and carry it.
	const shared = dnsmessage.ClassINET
	const unique = dnsmessage.Class(dnsClassIN | dnsCacheFlush)

	service, err := dnsmessage.NewName(esphomeService)
	if err != nil {
		return nil
	}
	instance, err := dnsmessage.NewName(m.serviceName())
	if err != nil {
		return nil
	}
	host, err := dnsmessage.NewName(m.hostName())
	if err != nil {
		return nil
	}

	build := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	err = build.StartAnswers()
	if err == nil {
		err = build.PTRResource(
			dnsmessage.ResourceHeader{Name: service, Class: shared, TTL: ttl},
			dnsmessage.PTRResource{PTR: instance})
	}
	if err == nil {
		err = build.StartAdditionals()
	}
	if err == nil {
		err = build.SRVResource(
			dnsmessage.ResourceHeader{Name: instance, Class: unique, TTL: min(ttl, hostTTL)},
			dnsmessage.SRVResource{Target: host, Port: m.Port})
	}
	if err == nil {
		err = build.TXTResource(
			dnsmessage.ResourceHeader{Name: instance, Class: unique, TTL: ttl},
			dnsmessage.TXTResource{TXT: m.txt()})
	}
	if ip := m.address(); err == nil && ip.To4() != nil {
		var a [4]byte
		copy(a[:], ip.To4())
		err = build.AResource(
			dnsmessage.ResourceHeader{Name: host, Class: unique, TTL: hostTTL},
			dnsmessage.AResource{A: a})
	}
	if err != nil {
		return nil
	}
	packet, err := build.Finish()
	if err != nil {
		return nil
	}
	return packet
}

func (m *Responder) txt() []string {
	mac := strings.ToLower(strings.ReplaceAll(m.MAC, ":", ""))
	return []string{
		"mac=" + mac,
		// What a real device publishes when it has a key, and the one TXT key
		// Home Assistant reads that decides whether it probes in plaintext first.
		"api_encryption=" + noiseCipherName,
		"version=" + esphomeVersion,
		"friendly_name=" + m.Instance,
		"platform=overdub",
		"board=biscuit",
		"network=wifi",
	}
}

// A goodbye is the same records at TTL zero: that is what retires the name.
// Any other TTL leaves it advertised for that long after the daemon is gone.
func (m *Responder) goodbyeRecords() []byte { return m.records(0) }

func (m *Responder) Goodbye() {
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	payload := m.goodbyeRecords()

	m.mu.Lock()
	m.gone = true
	conn := m.conn
	m.mu.Unlock()

	// A socket of its own when serve() has none, which is every second of the
	// thirty it backs off for after a failure and the five it waits an address
	// out. Without one a daemon told to stop during either withdraws nothing,
	// and Home Assistant holds the device for the PTR's full 4500 seconds.
	if conn == nil {
		opened, err := m.open()
		if err != nil {
			log.Printf("mdns: nothing to withdraw from (%v); the records expire on their own", err)
			return
		}
		defer opened.Close()
		conn = opened
	}

	// Twice a second apart, which is RFC 6762 section 10.1 and also the only
	// spacing that works: python-zeroconf drops a byte-identical packet inside
	// a one-second window, so a goodbye sent twice back to back is read once.
	// The deadline is per write, because this runs on the way out of the process
	// with the button still grabbed and a write that blocked would hold it.
	for i := 0; i < 2; i++ {
		if i > 0 {
			time.Sleep(goodbyeGap)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.WriteToUDP(payload, group); err != nil {
			return
		}
	}
}

func interfaceIPv4(name string) (net.IP, *net.IPNet, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, nil, err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4, ipnet, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("%s has no IPv4 address", name)
}
