//go:build linux

package mdns

import (
	"bytes"
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func firstIPv4(t *testing.T) (string, net.IP, *net.IPNet) {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interfaces: %v", err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if network, ok := addr.(*net.IPNet); ok && network.IP.To4() != nil {
				return iface.Name, network.IP.To4(), network
			}
		}
	}
	t.Skip("no multicast-capable IPv4 interface")
	return "", nil, nil
}

func TestServeClearsThePreviousCycleOnARealSocket(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", Iface: name, Services: testServices()}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	responder.restart = true
	responder.sendFailed = errors.New("network is unreachable")
	responder.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- responder.serve() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		responder.mu.Lock()
		restart, failed, conn := responder.restart, responder.sendFailed, responder.conn
		responder.mu.Unlock()
		if conn != nil && !restart && failed == nil {
			break
		}
		select {
		case err := <-done:
			if errors.Is(err, syscall.ENOPROTOOPT) {
				t.Skipf("no multicast socket in this environment: %v", err)
			}
			t.Fatalf("serve returned before it had a socket: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not clear the previous cycle: restart=%v failed=%v conn=%v",
				restart, failed, conn != nil)
		}
		time.Sleep(20 * time.Millisecond)
	}

	replyIsLive(t, responder)

	responder.Goodbye()
	responder.mu.Lock()
	if responder.conn != nil {
		responder.conn.Close()
	}
	responder.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("serve did not return after its socket was closed")
	}
}

func replyIsLive(t *testing.T, responder *Responder) {
	t.Helper()
	responder.mu.Lock()
	address := responder.ip
	responder.mu.Unlock()

	asker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: address, Port: 0})
	if err != nil {
		t.Fatalf("no asking socket: %v", err)
	}
	defer asker.Close()
	target := &net.UDPAddr{IP: address, Port: mdnsPort}

	question := query(1, []string{testService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse)
	if _, err := asker.WriteToUDP(question, target); err != nil {
		t.Fatalf("could not ask: %v", err)
	}

	if err := asker.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 9000)
	n, _, err := asker.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("the read loop sent no reply: %v", err)
	}

	live := false
	for _, record := range walkRecords(t, buffer[:n]) {
		if record.ttl == 0 {
			t.Errorf("type %d came back at TTL 0, which retires the record it is meant to advertise", record.rrType)
		}
		if record.rrType == dnsTypePTR && record.points == responder.instanceFor(testService) {
			live = true
		}
	}
	if !live {
		t.Errorf("the reply carried no PTR for %s", responder.instanceFor(testService))
	}
}

func TestGoodbyeOpensItsOwnSocketWhenServeHasNone(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", Iface: name, Services: testServices()}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	responder.mu.Unlock()

	if conn, err := responder.open(); err != nil {
		if errors.Is(err, syscall.ENOPROTOOPT) {
			t.Skipf("no multicast socket in this environment: %v", err)
		}
		t.Fatalf("open: %v", err)
	} else {
		conn.Close()
	}

	responder.mu.Lock()
	hasSocket := responder.conn != nil
	responder.mu.Unlock()
	if hasSocket {
		t.Fatal("this responder was never served, so it should hold no socket")
	}

	responder.Goodbye()

	responder.mu.Lock()
	gone := responder.gone
	responder.mu.Unlock()
	if !gone {
		t.Error("Goodbye did not mark the responder gone")
	}
}

func TestWatchAddressStopsWithTheSocket(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", Iface: name, Services: testServices()}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	responder.mu.Unlock()

	conn, err := responder.open()
	if err != nil {
		if errors.Is(err, syscall.ENOPROTOOPT) {
			t.Skipf("no multicast socket in this environment: %v", err)
		}
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		responder.watchAddress(conn, done)
		close(stopped)
	}()
	close(done)

	select {
	case <-stopped:
	case <-time.After(addressPoll):
		t.Fatalf("watchAddress outlived its socket; it would leak one goroutine per serve cycle "+
			"(goroutines were %d before it started)", before)
	}
}

func TestOpenSetsEverySocketOptionTheResponderDependsOn(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", Iface: name, Services: testServices()}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	responder.mu.Unlock()

	conn, err := responder.open()
	if err != nil {
		if errors.Is(err, syscall.ENOPROTOOPT) {
			t.Skipf("no multicast socket in this environment: %v", err)
		}
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var (
		reuseAddr, reusePort       int
		reuseAddrErr, reusePortErr error
		sendFrom                   [4]byte
		sendFromErr                error
	)
	if err := raw.Control(func(fd uintptr) {
		const soReusePort = 0xf
		reuseAddr, reuseAddrErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR)
		reusePort, reusePortErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort)
		sendFrom, sendFromErr = syscall.GetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF)
	}); err != nil {
		t.Fatal(err)
	}

	unreadable := func(err error) bool { return errors.Is(err, syscall.ENOPROTOOPT) }

	switch {
	case unreadable(reuseAddrErr):
		t.Log("SO_REUSEADDR cannot be read back here")
	case reuseAddrErr != nil:
		t.Errorf("SO_REUSEADDR: %v", reuseAddrErr)
	case reuseAddr == 0:
		t.Error("SO_REUSEADDR is not set; the bind fails wherever another responder already holds the port")
	}
	switch {
	case unreadable(reusePortErr):
		t.Log("SO_REUSEPORT cannot be read back here")
	case reusePortErr != nil:
		t.Errorf("SO_REUSEPORT: %v", reusePortErr)
	case reusePort == 0:
		t.Error("SO_REUSEPORT is not set; same bind, same failure")
	}
	switch got := net.IPv4(sendFrom[0], sendFrom[1], sendFrom[2], sendFrom[3]); {
	case unreadable(sendFromErr):
		t.Log("IP_MULTICAST_IF cannot be read back here")
	case sendFromErr != nil:
		t.Errorf("IP_MULTICAST_IF: %v", sendFromErr)
	case !got.Equal(ip):
		t.Errorf("IP_MULTICAST_IF is %v, want %v; replies would leave by whichever interface the route table prefers", got, ip)
	}
	if !joinedTheGroup(t, name) {
		t.Errorf("%s has not joined 224.0.0.251: the responder is deaf to every query Home Assistant sends", name)
	}
}

func joinedTheGroup(t *testing.T, iface string) bool {
	t.Helper()
	const mdnsGroupLittleEndian = "FB0000E0" // 224.0.0.251, as /proc prints it

	table, err := os.ReadFile("/proc/net/igmp")
	if err != nil {
		t.Skipf("no /proc/net/igmp to read membership from: %v", err)
	}
	ours := false
	for _, line := range strings.Split(string(table), "\n") {
		if !strings.HasPrefix(line, "\t") && strings.Contains(line, ":") {
			ours = strings.Contains(line, iface+" ") || strings.Contains(line, iface+":")
			continue
		}
		if ours && strings.Contains(strings.ToUpper(line), mdnsGroupLittleEndian) {
			return true
		}
	}
	return false
}

func TestTheReadLoopRepliesForTheServiceThatWasAskedFor(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", Iface: name, Services: twoServices()}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	responder.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- responder.serve() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		responder.mu.Lock()
		conn := responder.conn
		responder.mu.Unlock()
		if conn != nil {
			break
		}
		select {
		case err := <-done:
			if errors.Is(err, syscall.ENOPROTOOPT) {
				t.Skipf("no multicast socket in this environment: %v", err)
			}
			t.Fatalf("serve returned before it had a socket: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never opened a socket")
		}
		time.Sleep(20 * time.Millisecond)
	}

	defer func() {
		responder.Goodbye()
		responder.mu.Lock()
		if responder.conn != nil {
			responder.conn.Close()
		}
		responder.mu.Unlock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serve did not return after its socket was closed")
		}
	}()

	asker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		t.Fatalf("no asking socket: %v", err)
	}
	defer asker.Close()

	question := query(1, []string{secondService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse)
	if _, err := asker.WriteToUDP(question, &net.UDPAddr{IP: ip, Port: mdnsPort}); err != nil {
		t.Fatalf("could not ask: %v", err)
	}
	if err := asker.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 9000)
	n, _, err := asker.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("the read loop sent no reply: %v", err)
	}

	asked, unasked := false, false
	for _, record := range walkRecords(t, buffer[:n]) {
		if strings.EqualFold(record.name, secondService) ||
			strings.EqualFold(record.name, responder.instanceFor(secondService)) {
			asked = true
		}
		if strings.EqualFold(record.name, testService) ||
			strings.EqualFold(record.name, responder.instanceFor(testService)) {
			unasked = true
		}
	}
	if !asked {
		t.Errorf("the reply carried nothing for %s, which is what was asked for", secondService)
	}
	if unasked {
		t.Errorf("the reply carried records for %s, which nothing asked about", testService)
	}
}

func TestInterfaceIPv4FindsTheAddressTheResponderBindsTo(t *testing.T) {
	name, ip, _ := firstIPv4(t)
	got, subnet, err := interfaceIPv4(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !got.Equal(ip) {
		t.Errorf("%s gave %v, want %v", name, got, ip)
	}
	if got.IsLoopback() {
		t.Errorf("%s gave the loopback address %v", name, got)
	}
	if subnet == nil || !subnet.Contains(got) {
		t.Errorf("%s gave subnet %v, which does not contain %v", name, subnet, got)
	}
}

func countOurs(conn *net.UDPConn, instance string, window time.Duration) (int, error) {
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		return 0, err
	}
	seen := 0
	buffer := make([]byte, 9000)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return seen, nil
		}
		if bytes.Contains(buffer[:n], []byte(instance)) {
			seen++
		}
	}
}

func TestAnAnnouncementRepeatsAndAGoodbyeIsSentTwice(t *testing.T) {
	_, ip, subnet := firstIPv4(t)
	const instance = "overdub-selftest"
	responder := &Responder{Instance: instance, Services: testServices()}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	responder.mu.Unlock()

	listener, err := responder.open()
	if err != nil {
		if errors.Is(err, syscall.ENOPROTOOPT) {
			t.Skipf("no multicast socket in this environment: %v", err)
		}
		t.Fatalf("open a listening socket: %v", err)
	}
	defer listener.Close()

	sender, err := responder.open()
	if err != nil {
		t.Fatalf("open a sending socket: %v", err)
	}
	defer sender.Close()

	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

	responder.mu.Lock()
	responder.conn = sender
	responder.mu.Unlock()

	type count struct {
		seen int
		err  error
	}
	counted := make(chan count, 1)
	listen := func() {
		seen, err := countOurs(listener, instance, 5*time.Second)
		counted <- count{seen, err}
	}

	go listen()
	time.Sleep(200 * time.Millisecond)
	if err := responder.announce(sender, group); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if got := <-counted; got.err != nil {
		t.Fatalf("counting announcements: %v", got.err)
	} else if got.seen < 2 {
		t.Errorf("announce put %d packets on the wire, want at least 2: RFC 6762 asks for"+
			" the repeat a second later, and for the rungs after it", got.seen)
	}
	responder.stopLadder()

	go listen()
	time.Sleep(200 * time.Millisecond)
	responder.Goodbye()
	if got := <-counted; got.err != nil {
		t.Fatalf("counting goodbyes: %v", got.err)
	} else if got.seen != 2 {
		t.Errorf("Goodbye put %d packets on the wire, want 2", got.seen)
	}
}
