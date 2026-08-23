//go:build linux

package esphome

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serve() holds a socket, so everything it does per cycle is unreachable
// without one. That is why a whole set of lifecycle guards could be deleted
// with the suite still green. This binds a real multicast socket, which works
// wherever the daemon itself does.
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
	// Not "kitchen": this one really does announce on the network the test
	// machine is attached to, so its name must not be one anybody has given a
	// Dot. It withdraws itself at the end.
	responder := &Responder{Instance: "overdub-selftest", MAC: "00:00:5E:00:53:2A", Iface: name, Port: 6053}
	responder.mu.Lock()
	responder.ip, responder.subnet = ip, subnet
	// What the previous cycle would have left behind.
	responder.restart = true
	responder.sendFailed = errors.New("network is unreachable")
	responder.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- responder.serve() }()

	// serve() clears both before it blocks on the socket.
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
			// qemu-user does not translate IP_MULTICAST_IF, so the emulated
			// ARM run cannot open this socket at all. The native run in CI is
			// real Linux and does, which is where this test earns its keep.
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

	// The read loop is the one part of the reply path nothing else reaches:
	// replyTo is tested to death in isolation, and serve() is what turns its
	// verdict into bytes on the wire. A reply built at the wrong TTL retires
	// the record as fast as it advertises it, and the suite stays green.
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

	// Bound to the responder's own address and sent to its own socket: the
	// query never leaves the machine, and its source is on-link, which is what
	// replyTo requires before it answers anything.
	asker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: address, Port: 0})
	if err != nil {
		t.Fatalf("no asking socket: %v", err)
	}
	defer asker.Close()
	target := &net.UDPAddr{IP: address, Port: mdnsPort}

	question := query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse)
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
		if record.rrType == dnsTypePTR && record.points == responder.serviceName() {
			live = true
		}
	}
	if !live {
		t.Errorf("the reply carried no PTR for %s", responder.serviceName())
	}
}

// The branch that matters when the daemon is told to stop between sockets: the
// thirty seconds serve() backs off for, and the five it waits an address out.
// Without it a withdrawal in either window withdraws nothing, and Home
// Assistant holds the device for the PTR's full 4500 seconds.
func TestGoodbyeOpensItsOwnSocketWhenServeHasNone(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", MAC: "00:00:5E:00:53:2A", Iface: name, Port: 6053}
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

// watchAddress polls, and the poll has to be interruptible: one goroutine per
// serve cycle, each holding a socket that is already closed, for the life of a
// daemon that never returns.
func TestWatchAddressStopsWithTheSocket(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", MAC: "00:00:5E:00:53:2A", Iface: name, Port: 6053}
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

// Every one of these can be dropped without the daemon failing to start, and
// three of the four fail silently afterwards. Losing IP_ADD_MEMBERSHIP is the
// worst: the socket binds, the announcement still goes out, and the responder
// simply never hears a query again on a machine where nothing else has joined
// the group. Read back rather than sent, so the assertion costs no traffic.
func TestOpenSetsEverySocketOptionTheResponderDependsOn(t *testing.T) {
	name, ip, subnet := firstIPv4(t)
	responder := &Responder{Instance: "overdub-selftest", MAC: "00:00:5E:00:53:2A", Iface: name, Port: 6053}
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

	// Read back one at a time. IP_MULTICAST_IF cannot be read back at all --
	// Linux answers ENOPROTOOPT, measured natively and under qemu alike -- so
	// that one is reported and skipped rather than taken for a failure, and it
	// is the one socket option here that nothing asserts. The other three are.
	unreadable := func(err error) bool { return errors.Is(err, syscall.ENOPROTOOPT) }

	// Two sockets already hold this port on the Dot, so the bind needs both.
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

// The kernel's own record of who joined what. Membership cannot be read back
// with getsockopt, and asking on the wire would mean multicasting to find out.
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
