// Package mdns answers the multicast DNS queries a browser sends, for every
// service this device offers. It knows nothing about any of them.
package mdns

import (
	"context"
	"errors"
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

	goodbyeGap   = time.Second
	goodbyeWrite = 2 * time.Second
	addressPoll  = 30 * time.Second

	announceFirst = time.Second
	announceRungs = 6

	multicastEvery = time.Second

	unicastBurst  = 20
	unicastWindow = time.Second

	dnssdMeta = "_services._dns-sd._udp.local."

	maxTXTString = 255
	maxLabel     = 63
	maxName      = 255
)

type Advert struct {
	Service string
	Port    uint16
	Records []string
}

type answer struct {
	adverts []served
	host    bool
	noHost  bool
}

type served struct {
	Advert
	ptr     bool
	resolve bool
}

type Responder struct {
	Instance string
	Iface    string
	Services []Advert

	mu         sync.Mutex
	services   []Advert
	conn       *net.UDPConn
	ip         net.IP
	subnet     *net.IPNet
	restart    bool
	lastSent   time.Time
	unicastEnd time.Time
	unicasts   int
	sendFailed error
	gone       bool
	ladder     chan struct{}
	rungs      []time.Duration
}

func (m *Responder) address() net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ip
}

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

func (m *Responder) instanceFor(service string) string { return m.Instance + "." + service }

func (m *Responder) adverts() []Advert {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.services == nil {
		m.services = append([]Advert(nil), m.Services...)
	}
	return m.services
}

func (m *Responder) Advertise(list []Advert) error {
	if err := m.checkAdverts(list); err != nil {
		return err
	}
	was := m.adverts()

	m.mu.Lock()
	m.services = append([]Advert(nil), list...)
	conn, gone := m.conn, m.gone
	m.mu.Unlock()

	if conn == nil || gone {
		return nil
	}
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	if dropped := missingFrom(was, list); len(dropped) > 0 {
		m.stopLadder()
		if payload := m.withdrawal(dropped); payload != nil {
			for i := 0; i < 2; i++ {
				if i > 0 {
					time.Sleep(goodbyeGap)
				}
				_ = conn.SetWriteDeadline(time.Now().Add(goodbyeWrite))
				err := m.writeTo(conn, payload, group)
				_ = conn.SetWriteDeadline(time.Time{})
				if err != nil {
					break
				}
			}
		}
	}
	if len(missingFrom(list, was)) > 0 {
		return m.announce(conn, group)
	}
	return nil
}

func (m *Responder) withdrawal(dropped []Advert) []byte {
	send := answer{adverts: make([]served, 0, len(dropped)), noHost: true}
	for _, a := range dropped {
		send.adverts = append(send.adverts, served{Advert: a, ptr: true, resolve: true})
	}
	return m.recordsFor(0, send)
}

func missingFrom(from, in []Advert) []Advert {
	var out []Advert
	for _, a := range from {
		found := false
		for _, b := range in {
			if strings.EqualFold(a.Service, b.Service) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, a)
		}
	}
	return out
}
func (m *Responder) hostName() string { return m.Instance + ".local." }

func (m *Responder) Run() {
	if err := m.checkRecords(); err != nil {
		log.Printf("mdns: %v; not advertising", err)
		return
	}
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
	for _, a := range m.adverts() {
		log.Printf("mdns: announcing %s at %s:%d", m.instanceFor(a.Service), ip, a.Port)
	}
	go func() {
		if err := m.announce(conn, group); err != nil {
			select {
			case <-done:
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
		dst, send, wanted := m.replyTo(src, group, buffer[:n])
		if !wanted {
			continue
		}
		if err := m.writeTo(conn, m.recordsFor(ttlShared, send), dst); err != nil {
			m.noteReplyFailure(dst, group, err)
		}
	}
}

var errNothingToSend = errors.New("built no records to send")

func (m *Responder) writeTo(conn *net.UDPConn, payload []byte, dst *net.UDPAddr) error {
	if len(payload) == 0 {
		return errNothingToSend
	}
	_, err := conn.WriteToUDP(payload, dst)
	return err
}

func (m *Responder) beginCycle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restart, m.sendFailed = false, nil
}

func (m *Responder) replyTo(src, group *net.UDPAddr, packet []byte) (*net.UDPAddr, answer, bool) {
	if !m.onLink(src.IP) {
		return nil, answer{}, false
	}
	send, unicast, wanted := m.wants(packet)
	if !wanted {
		return nil, answer{}, false
	}
	m.mu.Lock()
	withdrawn := m.gone
	m.mu.Unlock()
	if withdrawn {
		return nil, answer{}, false
	}
	if unicast {
		if !m.mayUnicast() {
			return nil, answer{}, false
		}
		return src, send, true
	}
	if !m.mayMulticast() {
		return nil, answer{}, false
	}
	return group, send, true
}

func (m *Responder) announce(conn *net.UDPConn, group *net.UDPAddr) error {
	if err := m.announceOnce(conn, group); err != nil {
		return err
	}
	m.startLadder(conn, group)
	return nil
}

func (m *Responder) announceOnce(conn *net.UDPConn, group *net.UDPAddr) error {
	payload := m.records(ttlShared)

	m.mu.Lock()
	gone := m.gone
	m.lastSent = time.Now()
	m.mu.Unlock()
	if gone {
		return nil
	}
	return m.writeTo(conn, payload, group)
}

func (m *Responder) announceSchedule() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rungs != nil {
		return append([]time.Duration(nil), m.rungs...)
	}
	out := make([]time.Duration, 0, announceRungs)
	for d := announceFirst; len(out) < announceRungs; d *= 2 {
		out = append(out, d)
	}
	return out
}

func (m *Responder) startLadder(conn *net.UDPConn, group *net.UDPAddr) {
	schedule := m.announceSchedule()
	stop := make(chan struct{})

	m.mu.Lock()
	if m.ladder != nil {
		close(m.ladder)
	}
	m.ladder = stop
	m.mu.Unlock()

	go func() {
		for _, d := range schedule {
			select {
			case <-stop:
				return
			case <-time.After(d):
			}
			m.mu.Lock()
			moved := m.conn != conn
			gone := m.gone
			m.mu.Unlock()
			if moved || gone {
				return
			}
			if err := m.announceOnce(conn, group); err != nil {
				m.noteSendFailure(err)
				return
			}
		}
	}()
}

func (m *Responder) stopLadder() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ladder != nil {
		close(m.ladder)
		m.ladder = nil
	}
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

func (m *Responder) wants(packet []byte) (send answer, unicast bool, wanted bool) {
	var parser dnsmessage.Parser
	header, err := parser.Start(packet)
	if err != nil || header.Response {
		return answer{}, false, false
	}

	adverts := m.adverts()
	browse := make([]bool, len(adverts))
	resolve := make([]bool, len(adverts))
	matched := false
	var asked struct{ unicast, multicast bool }
	for {
		question, err := parser.Question()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return answer{}, false, false
		}
		name := strings.ToLower(question.Name.String())
		qtype := uint16(question.Type)
		ours := false
		if name == strings.ToLower(m.hostName()) && (qtype == dnsTypeA || qtype == dnsTypeANY) {
			ours, send.host = true, m.address() != nil
		}
		for i, a := range adverts {
			if name == strings.ToLower(a.Service) && (qtype == dnsTypePTR || qtype == dnsTypeANY) {
				ours, browse[i] = true, true
			}
			if name == strings.ToLower(m.instanceFor(a.Service)) &&
				(qtype == dnsTypeSRV || qtype == dnsTypeTXT || qtype == dnsTypeANY) {
				ours, resolve[i] = true, true
			}
		}
		if ours {
			matched = true
			if uint16(question.Class)&dnsUnicastResponse != 0 {
				asked.unicast = true
			} else {
				asked.multicast = true
			}
		}
	}
	if !matched {
		return answer{}, false, false
	}

	known := m.knownAnswers(&parser, adverts)
	for i, a := range adverts {
		ptr := browse[i] && !known[i]
		if !ptr && !resolve[i] {
			continue
		}
		send.adverts = append(send.adverts, served{Advert: a, ptr: ptr, resolve: resolve[i]})
	}
	if len(send.adverts) == 0 && !send.host {
		return answer{}, false, false
	}
	return send, asked.unicast && !asked.multicast, true
}

func (m *Responder) knownAnswers(parser *dnsmessage.Parser, adverts []Advert) []bool {
	known := make([]bool, len(adverts))
	for {
		header, err := parser.AnswerHeader()
		if err != nil {
			return known
		}
		if header.Type != dnsmessage.TypePTR {
			if parser.SkipAnswer() != nil {
				return known
			}
			continue
		}
		ptr, err := parser.PTRResource()
		if err != nil {
			return known
		}
		if header.TTL < ttlShared/2 {
			continue
		}
		for i, a := range adverts {
			if strings.EqualFold(header.Name.String(), a.Service) &&
				strings.EqualFold(ptr.PTR.String(), m.instanceFor(a.Service)) {
				known[i] = true
			}
		}
	}
}

func (m *Responder) records(ttl uint32) []byte {
	adverts := m.adverts()
	send := answer{adverts: make([]served, 0, len(adverts))}
	for _, a := range adverts {
		send.adverts = append(send.adverts, served{Advert: a, ptr: true})
	}
	return m.recordsFor(ttl, send)
}

func (m *Responder) recordsFor(ttl uint32, send answer) []byte {
	adverts := send.adverts
	hostTTL := ttl
	if hostTTL > ttlHost {
		hostTTL = ttlHost
	}
	const shared = dnsmessage.ClassINET
	const unique = dnsmessage.Class(dnsClassIN | dnsCacheFlush)

	host, err := dnsmessage.NewName(m.hostName())
	if err != nil {
		return nil
	}
	names := make([][2]dnsmessage.Name, 0, len(adverts))
	for _, a := range adverts {
		service, err := dnsmessage.NewName(a.Service)
		if err != nil {
			return nil
		}
		instance, err := dnsmessage.NewName(m.instanceFor(a.Service))
		if err != nil {
			return nil
		}
		names = append(names, [2]dnsmessage.Name{service, instance})
	}

	build := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	aRecord := func() error {
		ip := m.address()
		if ip == nil || ip.To4() == nil {
			return nil
		}
		var a [4]byte
		copy(a[:], ip.To4())
		return build.AResource(
			dnsmessage.ResourceHeader{Name: host, Class: unique, TTL: hostTTL},
			dnsmessage.AResource{A: a})
	}
	serviceRecords := func(i int, a served) error {
		if err := build.SRVResource(
			dnsmessage.ResourceHeader{Name: names[i][1], Class: unique, TTL: min(ttl, hostTTL)},
			dnsmessage.SRVResource{Target: host, Port: a.Port}); err != nil {
			return err
		}
		return build.TXTResource(
			dnsmessage.ResourceHeader{Name: names[i][1], Class: unique, TTL: ttl},
			dnsmessage.TXTResource{TXT: a.Records})
	}

	err = build.StartAnswers()
	for i, a := range adverts {
		if err != nil {
			break
		}
		if a.ptr {
			err = build.PTRResource(
				dnsmessage.ResourceHeader{Name: names[i][0], Class: shared, TTL: ttl},
				dnsmessage.PTRResource{PTR: names[i][1]})
		}
	}
	for i, a := range adverts {
		if err != nil {
			break
		}
		if a.resolve {
			err = serviceRecords(i, a)
		}
	}
	if err == nil && send.host {
		err = aRecord()
	}

	if err == nil {
		err = build.StartAdditionals()
	}
	for i, a := range adverts {
		if err != nil {
			break
		}
		if !a.resolve {
			err = serviceRecords(i, a)
		}
	}
	if err == nil && !send.host && !send.noHost {
		err = aRecord()
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

var errNoRecords = errors.New("a responder needs a service, a port and TXT records")

var errUnbuildable = errors.New("a responder's names and records must fit the wire format")

func validName(name string) error {
	if !strings.HasSuffix(name, ".") {
		return fmt.Errorf("%q does not end in a dot", name)
	}
	if len(name) > maxName {
		return fmt.Errorf("%q is %d bytes, over %d", name, len(name), maxName)
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			return fmt.Errorf("%q has an empty label", name)
		}
		if len(label) > maxLabel {
			return fmt.Errorf("%q has a %d-byte label, over %d", name, len(label), maxLabel)
		}
	}
	return nil
}

func (m *Responder) checkRecords() error { return m.checkAdverts(m.adverts()) }

func (m *Responder) checkAdverts(list []Advert) error {
	if len(list) == 0 {
		return fmt.Errorf("%w: none were given", errNoRecords)
	}
	for _, a := range list {
		if a.Service == "" || len(a.Records) == 0 || a.Port == 0 {
			return fmt.Errorf("%w: %q", errNoRecords, a.Service)
		}
	}
	if err := validName(m.hostName()); err != nil {
		return fmt.Errorf("%w: host %w", errUnbuildable, err)
	}
	seen := map[string]bool{}
	for _, a := range list {
		if err := validName(a.Service); err != nil {
			return fmt.Errorf("%w: %w", errUnbuildable, err)
		}
		if err := validName(m.instanceFor(a.Service)); err != nil {
			return fmt.Errorf("%w: %w", errUnbuildable, err)
		}
		if seen[strings.ToLower(a.Service)] {
			return fmt.Errorf("%w: %q is carried twice, so one instance would answer with two ports",
				errNoRecords, a.Service)
		}
		seen[strings.ToLower(a.Service)] = true
		for _, r := range a.Records {
			if len(r) > maxTXTString {
				return fmt.Errorf("%w: %q: a TXT string is %d bytes, over %d",
					errUnbuildable, a.Service, len(r), maxTXTString)
			}
		}
	}
	dry := answer{adverts: make([]served, 0, len(list)), host: true}
	for _, a := range list {
		dry.adverts = append(dry.adverts, served{Advert: a, ptr: true, resolve: true})
	}
	if m.recordsFor(ttlShared, dry) == nil {
		return fmt.Errorf("%w: nothing this responder carries can be packed", errUnbuildable)
	}
	return nil
}

func (m *Responder) goodbyeRecords() []byte { return m.records(0) }

func (m *Responder) Goodbye() {
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	payload := m.goodbyeRecords()

	m.mu.Lock()
	m.gone = true
	conn := m.conn
	m.mu.Unlock()

	if conn == nil {
		opened, err := m.open()
		if err != nil {
			log.Printf("mdns: nothing to withdraw from (%v); the records expire on their own", err)
			return
		}
		defer opened.Close()
		conn = opened
	}

	for i := 0; i < 2; i++ {
		if i > 0 {
			time.Sleep(goodbyeGap)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(goodbyeWrite))
		if err := m.writeTo(conn, payload, group); err != nil {
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
