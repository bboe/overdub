package mdns

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const testService = "_esphomelib._tcp.local."

func testRecords() []string {
	return []string{"mac=00005e00532a", "api_encryption=Noise_NNpsk0_25519_ChaChaPoly_SHA256",
		"version=2026.8.0", "friendly_name=kitchen", "platform=overdub"}
}

func testServices() []Advert {
	return []Advert{{Service: testService, Port: 6053, Records: testRecords()}}
}

func TestRecordsFollowTheCurrentAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}

	first := net.IPv4(192, 0, 2, 48)
	responder.ip = first
	before := responder.records(ttlShared)
	if !bytes.Contains(before, first.To4()) {
		t.Fatal("records did not advertise the address it was given")
	}

	second := net.IPv4(192, 0, 2, 43)
	responder.ip = second
	after := responder.records(ttlShared)
	if !bytes.Contains(after, second.To4()) {
		t.Error("records did not pick up the new address")
	}
	if bytes.Contains(after, first.To4()) {
		t.Error("records still carries the old address")
	}
}

func TestRecordsOmitsAWithNoAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}

	without := binary.BigEndian.Uint16(responder.records(ttlShared)[10:12])
	responder.ip = net.IPv4(192, 0, 2, 43)
	with := binary.BigEndian.Uint16(responder.records(ttlShared)[10:12])

	if with != without+1 {
		t.Errorf("additional records: %d without an address, %d with; want one more", without, with)
	}
}

func encodeName(name string) []byte {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			continue
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}

func query(questions int, names []string, qtype, qclass uint16) []byte {
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], uint16(questions))
	for _, name := range names {
		packet = append(packet, encodeName(name)...)
		packet = append(packet, 0, 0, 0, 0)
		binary.BigEndian.PutUint16(packet[len(packet)-4:len(packet)-2], qtype)
		binary.BigEndian.PutUint16(packet[len(packet)-2:], qclass)
	}
	return packet
}

func TestWantsMatchesTheServiceAndHost(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices(), ip: net.IPv4(192, 0, 2, 40)}
	tests := []struct {
		name  string
		qname string
		qtype uint16
		want  bool
	}{
		{"service PTR", testService, dnsTypePTR, true},
		{"service ANY", testService, dnsTypeANY, true},
		{"service A is not ours to answer", testService, dnsTypeA, false},
		{"meta-query PTR", dnssdMeta, dnsTypePTR, false},
		{"meta-query SRV", dnssdMeta, dnsTypeSRV, false},
		{"instance SRV", "kitchen." + testService, dnsTypeSRV, true},
		{"instance TXT", "kitchen." + testService, dnsTypeTXT, true},
		{"host A", "kitchen.local.", dnsTypeA, true},
		{"host SRV", "kitchen.local.", dnsTypeSRV, false},
		{"another device's host", "bedroom.local.", dnsTypeA, false},
		{"case is ignored", "KITCHEN.LOCAL.", dnsTypeA, true},
	}
	for _, tt := range tests {
		_, _, got := responder.wants(query(1, []string{tt.qname}, tt.qtype, dnsClassIN))
		if got != tt.want {
			t.Errorf("%s: wants(%q, %d) = %v, want %v", tt.name, tt.qname, tt.qtype, got, tt.want)
		}
	}
}

func TestWantsReportsTheUnicastBit(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}

	_, unicast, wanted := responder.wants(query(1, []string{testService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse))
	if !wanted || !unicast {
		t.Errorf("unicast bit set: got unicast=%v wanted=%v, want true true", unicast, wanted)
	}
	_, unicast, wanted = responder.wants(query(1, []string{testService}, dnsTypePTR, dnsClassIN))
	if !wanted || unicast {
		t.Errorf("unicast bit clear: got unicast=%v wanted=%v, want false true", unicast, wanted)
	}
}

func TestWantsScansEveryQuestion(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	packet := query(2, []string{"unrelated.local.", testService}, dnsTypePTR, dnsClassIN)
	if _, _, wanted := responder.wants(packet); !wanted {
		t.Error("a match in the second question was missed")
	}
}

func TestWantsRejectsMalformed(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	good := query(1, []string{testService}, dnsTypePTR, dnsClassIN)

	tests := []struct {
		name   string
		packet []byte
	}{
		{"empty", nil},
		{"shorter than a header", good[:11]},
		{"header only, QDCOUNT 1", good[:12]},
		{"QDCOUNT claims more than it carries", query(9, []string{"unrelated.local."}, dnsTypePTR, dnsClassIN)},
		{"name runs past the packet", good[:len(good)-6]},
		{"type and class truncated", good[:len(good)-3]},
	}
	for _, tt := range tests {
		if _, _, wanted := responder.wants(tt.packet); wanted {
			t.Errorf("%s: wants() said yes to a malformed packet", tt.name)
		}
	}
}

func TestWantsSurvivesWhatTheWireFormatForbids(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}

	header := func(questions, answers int) []byte {
		packet := make([]byte, 12)
		binary.BigEndian.PutUint16(packet[4:6], uint16(questions))
		binary.BigEndian.PutUint16(packet[6:8], uint16(answers))
		return packet
	}
	question := append(encodeName(testService), 0, dnsTypePTR, 0, dnsClassIN)

	tests := []struct {
		name     string
		mayReply bool
		packet   []byte
	}{
		{"reserved label length", false, append(header(1, 0), append([]byte{0x80}, bytes.Repeat([]byte{'a'}, 0x80)...)...)},
		{"name over the 255 byte limit", false, append(header(1, 0), bytes.Repeat(append([]byte{63}, bytes.Repeat([]byte{'a'}, 63)...), 6)...)},
		{"pointer to itself", false, append(header(1, 0), 0xc0, 12)},
		{"pointer past the end", false, append(header(1, 0), 0xc0, 0xff)},
		{"truncated pointer", false, append(header(1, 0), 0xc0)},
		{"question count past the end", false, append(header(64, 0), question...)},
		{"label runs past the end", false, append(header(1, 0), 63, 'a')},
		{"header alone", false, header(1, 0)},
		{"empty", false, nil},
		{"answer count past the end", true, append(header(1, 64), question...)},
	}
	for _, tt := range tests {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panicked: %v", tt.name, r)
				}
			}()
			if _, _, wanted := responder.wants(tt.packet); wanted && !tt.mayReply {
				t.Errorf("%s: answered a query the wire format forbids", tt.name)
			}
		}()
	}
}

func TestWantsIgnoresResponses(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	packet := query(1, []string{testService}, dnsTypePTR, dnsClassIN)
	packet[2] |= 0x80 // QR
	if _, _, wanted := responder.wants(packet); wanted {
		t.Error("answered a response rather than a query")
	}
}

func TestWantsFollowsACompressedQuestionName(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}

	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], 2)
	packet = append(packet, encodeName(testService)...)
	packet = append(packet, 0, dnsTypeA, 0, dnsClassIN) // not a type we answer
	packet = append(packet, 0xc0, 12)                   // the same name, by pointer
	packet = append(packet, 0, dnsTypePTR, 0, dnsClassIN)

	if _, _, wanted := responder.wants(packet); !wanted {
		t.Error("a compressed question name was not matched")
	}
}

func TestOnLinkGatesTheSubnet(t *testing.T) {
	_, subnet, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	if responder.onLink(net.ParseIP("192.0.2.5")) {
		t.Error("answered with no address of our own yet")
	}
	responder.ip, responder.subnet = net.ParseIP("192.0.2.10"), subnet
	if !responder.onLink(net.ParseIP("192.0.2.5")) {
		t.Error("refused an address on our own subnet")
	}
	if responder.onLink(net.ParseIP("198.51.100.5")) {
		t.Error("answered a query from off-link")
	}
}

func TestStaleRebuildsAfterASendFailedAtTheSameAddress(t *testing.T) {
	address := net.IPv4(192, 0, 2, 44)
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}
	responder.ip = address

	if _, rebuild := responder.stale(address); rebuild {
		t.Fatal("rebuilt with nothing wrong")
	}
	responder.noteSendFailure(errors.New("network is unreachable"))
	reason, rebuild := responder.stale(address)
	if !rebuild {
		t.Fatal("a failed send at an unchanged address did not rebuild")
	}
	if !strings.Contains(reason, "send failed") {
		t.Errorf("reason = %q, want it to name the send", reason)
	}
	if !strings.Contains(reason, "network is unreachable") {
		t.Errorf("reason = %q, want it to carry the error", reason)
	}
}

func TestStaleClearsTheSendFlag(t *testing.T) {
	address := net.IPv4(192, 0, 2, 44)
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}
	responder.ip = address

	responder.noteSendFailure(errors.New("network is unreachable"))
	if _, rebuild := responder.stale(address); !rebuild {
		t.Fatal("first poll did not rebuild")
	}
	if _, rebuild := responder.stale(address); rebuild {
		t.Error("second poll rebuilt again on the same failure")
	}
}

func TestStaleStillRebuildsOnAChangedAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}
	responder.ip = net.IPv4(192, 0, 2, 44)

	reason, rebuild := responder.stale(net.IPv4(192, 0, 2, 48))
	if !rebuild {
		t.Fatal("a changed address did not rebuild")
	}
	if !strings.Contains(reason, "changed to") {
		t.Errorf("reason = %q, want it to name the change", reason)
	}
}

type parsedRecord struct {
	name   string
	rrType uint16
	class  uint16
	ttl    uint32
	port   uint16
	points string
}

func walkRecords(t *testing.T, packet []byte) []parsedRecord {
	t.Helper()
	var parser dnsmessage.Parser
	if _, err := parser.Start(packet); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		t.Fatalf("questions: %v", err)
	}

	var resources []dnsmessage.Resource
	for _, section := range []func() ([]dnsmessage.Resource, error){
		parser.AllAnswers, parser.AllAuthorities, parser.AllAdditionals,
	} {
		got, err := section()
		if err != nil && err != dnsmessage.ErrSectionDone {
			t.Fatalf("section: %v", err)
		}
		resources = append(resources, got...)
	}

	var out []parsedRecord
	for _, resource := range resources {
		record := parsedRecord{
			name:   resource.Header.Name.String(),
			rrType: uint16(resource.Header.Type),
			class:  uint16(resource.Header.Class),
			ttl:    resource.Header.TTL,
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.SRVResource:
			record.port = body.Port
			record.points = body.Target.String()
		case *dnsmessage.PTRResource:
			record.points = body.PTR.String()
		}
		out = append(out, record)
	}
	return out
}

func TestEveryRecordIsOwnedByTheNameThatShouldOwnIt(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43).To4()
	responder.mu.Unlock()

	want := map[uint16]struct{ name, points string }{
		dnsTypePTR: {testService, responder.instanceFor(testService)},
		dnsTypeSRV: {responder.instanceFor(testService), responder.hostName()},
		dnsTypeTXT: {responder.instanceFor(testService), ""},
		dnsTypeA:   {responder.hostName(), ""},
	}
	seen := map[uint16]bool{}
	for _, record := range walkRecords(t, responder.records(ttlShared)) {
		expect, known := want[record.rrType]
		if !known {
			t.Errorf("unexpected record of type %d", record.rrType)
			continue
		}
		if record.name != expect.name {
			t.Errorf("type %d is owned by %q, want %q", record.rrType, record.name, expect.name)
		}
		if record.points != expect.points {
			t.Errorf("type %d points at %q, want %q", record.rrType, record.points, expect.points)
		}
		seen[record.rrType] = true
	}
	for rrType := range want {
		if !seen[rrType] {
			t.Errorf("no record of type %d in the announcement", rrType)
		}
	}
}

func TestTheWireConstantsAreTheOnesHomeAssistantUses(t *testing.T) {
	if testService != "_esphomelib._tcp.local." {
		t.Errorf("testService = %q; Home Assistant browses for _esphomelib._tcp.local.", testService)
	}
	if dnssdMeta != "_services._dns-sd._udp.local." {
		t.Errorf("dnssdMeta = %q, want the RFC 6763 meta-query name", dnssdMeta)
	}
	if mdnsPort != 5353 {
		t.Errorf("mdnsPort = %d, want 5353", mdnsPort)
	}
	if goodbyeGap != time.Second {
		t.Errorf("goodbyeGap = %v, want 1s; python-zeroconf suppresses duplicates inside that window", goodbyeGap)
	}
	if ttlShared != 4500 || ttlHost != 120 {
		t.Errorf("TTLs are %d and %d, want RFC 6762's 4500 and 120", ttlShared, ttlHost)
	}
	if dnsClassIN != 1 || dnsCacheFlush != 0x8000 || dnsUnicastResponse != 0x8000 {
		t.Error("a class bit does not match the wire format")
	}
	types := map[string]uint16{"A": dnsTypeA, "PTR": dnsTypePTR, "TXT": dnsTypeTXT, "SRV": dnsTypeSRV, "ANY": dnsTypeANY}
	for name, want := range map[string]uint16{"A": 1, "PTR": 12, "TXT": 16, "SRV": 33, "ANY": 255} {
		if types[name] != want {
			t.Errorf("%s is %d, want %d", name, types[name], want)
		}
	}
}

func TestGoodbyeRetiresEveryRecord(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	responder.ip = net.IPv4(192, 0, 2, 43)

	records := walkRecords(t, responder.goodbyeRecords())
	if len(records) < 4 {
		t.Fatalf("goodbye carried %d records, want the PTR, SRV, TXT and A", len(records))
	}
	for _, r := range records {
		if r.ttl != 0 {
			t.Errorf("type %d has TTL %d in a goodbye; a non-zero TTL leaves the name "+
				"advertised for that long after the daemon is gone", r.rrType, r.ttl)
		}
	}
}

func TestSRVAndACarryTheShortHostTTL(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	responder.ip = net.IPv4(192, 0, 2, 43)

	for _, r := range walkRecords(t, responder.records(ttlShared)) {
		switch r.rrType {
		case dnsTypeSRV, dnsTypeA:
			if r.ttl != ttlHost {
				t.Errorf("type %d has TTL %d, want %d", r.rrType, r.ttl, ttlHost)
			}
		case dnsTypePTR, dnsTypeTXT:
			if r.ttl != ttlShared {
				t.Errorf("type %d has TTL %d, want %d", r.rrType, r.ttl, ttlShared)
			}
		}
	}
}

func queryWithKnownAnswer(name, alias string, ttl uint32) []byte {
	return queryWithKnownAnswerOwnedBy(name, name, alias, ttl)
}

func queryWithKnownAnswerOwnedBy(question, owner, alias string, ttl uint32) []byte {
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	binary.BigEndian.PutUint16(packet[6:8], 1)
	packet = append(packet, encodeName(question)...)
	packet = append(packet, 0, 0, 0, 0)
	binary.BigEndian.PutUint16(packet[len(packet)-4:len(packet)-2], dnsTypePTR)
	binary.BigEndian.PutUint16(packet[len(packet)-2:], dnsClassIN)

	rdata := encodeName(alias)
	packet = append(packet, encodeName(owner)...)
	head := make([]byte, 10)
	binary.BigEndian.PutUint16(head[0:2], dnsTypePTR)
	binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
	binary.BigEndian.PutUint32(head[4:8], ttl)
	binary.BigEndian.PutUint16(head[8:10], uint16(len(rdata)))
	packet = append(packet, head...)
	packet = append(packet, rdata...)
	return packet
}

func TestAKnownAnswerSuppressesTheReplyWhereverTheQuestionSits(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	others := []string{"_printer._tcp.local.", "_airplay._tcp.local."}

	for position := 0; position <= len(others); position++ {
		names := append(append([]string{}, others[:position]...), testService)
		names = append(names, others[position:]...)

		packet := make([]byte, 12)
		binary.BigEndian.PutUint16(packet[4:6], uint16(len(names)))
		binary.BigEndian.PutUint16(packet[6:8], 1)
		for _, name := range names {
			packet = append(packet, encodeName(name)...)
			packet = append(packet, 0, 0, 0, 0)
			binary.BigEndian.PutUint16(packet[len(packet)-4:len(packet)-2], dnsTypePTR)
			binary.BigEndian.PutUint16(packet[len(packet)-2:], dnsClassIN)
		}
		rdata := encodeName(responder.instanceFor(testService))
		packet = append(packet, encodeName(testService)...)
		head := make([]byte, 10)
		binary.BigEndian.PutUint16(head[0:2], dnsTypePTR)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		binary.BigEndian.PutUint32(head[4:8], ttlShared)
		binary.BigEndian.PutUint16(head[8:10], uint16(len(rdata)))
		packet = append(packet, head...)
		packet = append(packet, rdata...)

		if _, _, wanted := responder.wants(packet); wanted {
			t.Errorf("replied to a query carrying our own PTR, with our question at position %d of %d",
				position+1, len(names))
		}
	}
}

func TestARealBrowseIsAnsweredWhereverOurQuestionSits(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	const types = 90

	for ours := 0; ours < types; ours++ {
		packet := make([]byte, 12)
		binary.BigEndian.PutUint16(packet[4:6], types)
		for i := 0; i < types; i++ {
			name := fmt.Sprintf("_svc%03d._tcp.local.", i)
			if i == ours {
				name = testService
			}
			packet = append(packet, encodeName(name)...)
			packet = append(packet, 0, 0, 0, 0)
			binary.BigEndian.PutUint16(packet[len(packet)-4:len(packet)-2], dnsTypePTR)
			binary.BigEndian.PutUint16(packet[len(packet)-2:], dnsClassIN)
		}
		if _, _, wanted := responder.wants(packet); !wanted {
			t.Fatalf("refused a %d-question browse with our question at index %d", types, ours)
		}
	}
}

func TestTheHeaderAndTheClassesAreWhatZeroconfReads(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43).To4()
	responder.mu.Unlock()

	packet := responder.records(ttlShared)
	if flags := binary.BigEndian.Uint16(packet[2:4]); flags != 0x8400 {
		t.Errorf("header flags = %#04x, want %#04x (response, authoritative)", flags, 0x8400)
	}

	want := map[uint16]uint16{
		dnsTypePTR: dnsClassIN,                 // shared: every Dot answers this name
		dnsTypeSRV: dnsClassIN | dnsCacheFlush, // unique: ours alone
		dnsTypeTXT: dnsClassIN | dnsCacheFlush,
		dnsTypeA:   dnsClassIN | dnsCacheFlush,
	}
	seen := map[uint16]bool{}
	for _, record := range walkRecords(t, packet) {
		if record.class != want[record.rrType] {
			t.Errorf("type %d class = %#04x, want %#04x", record.rrType, record.class, want[record.rrType])
		}
		if record.rrType == dnsTypeSRV && record.port != responder.Services[0].Port {
			t.Errorf("SRV port = %d, want %d", record.port, responder.Services[0].Port)
		}
		seen[record.rrType] = true
	}
	for rrType := range want {
		if !seen[rrType] {
			t.Errorf("no record of type %d in the announcement", rrType)
		}
	}
}

func TestAKnownAnswerSuppressesTheReply(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}

	full := queryWithKnownAnswer(testService, responder.instanceFor(testService), ttlShared)
	if _, _, wanted := responder.wants(full); wanted {
		t.Error("answered a query that already carried our PTR at the full TTL")
	}
	stale := queryWithKnownAnswer(testService, responder.instanceFor(testService), ttlShared/2-1)
	if _, _, wanted := responder.wants(stale); !wanted {
		t.Error("suppressed on a known answer that had aged past half its TTL")
	}
	other := queryWithKnownAnswer(testService, "elsewhere."+testService, ttlShared)
	if _, _, wanted := responder.wants(other); !wanted {
		t.Error("suppressed on another device's known answer")
	}
	elsewhere := queryWithKnownAnswerOwnedBy(testService, "_printer._tcp.local.", responder.instanceFor(testService), ttlShared)
	if _, _, wanted := responder.wants(elsewhere); !wanted {
		t.Error("suppressed on a known answer owned by a service that is not ours")
	}
}

func TestTheMulticastRateLimitIsOneASecond(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}

	if !responder.mayMulticast() {
		t.Fatal("the first multicast was refused")
	}
	if responder.mayMulticast() {
		t.Error("a second multicast inside the window was allowed")
	}
	responder.mu.Lock()
	responder.lastSent = time.Now().Add(-2 * multicastEvery)
	responder.mu.Unlock()
	if !responder.mayMulticast() {
		t.Error("a multicast a full window later was refused")
	}
}

func TestServeClearsTheFlagsOfThePreviousCycle(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43)
	responder.restart = true
	responder.sendFailed = errors.New("network is unreachable")
	responder.mu.Unlock()

	responder.beginCycle()

	responder.mu.Lock()
	stillRestart, stillFailed := responder.restart, responder.sendFailed
	responder.mu.Unlock()
	if stillRestart || stillFailed != nil {
		t.Fatal("beginCycle did not clear both flags")
	}
	if _, rebuild := responder.stale(net.IPv4(192, 0, 2, 43)); rebuild {
		t.Error("a fresh socket was torn down by the previous cycle's failure")
	}
}

func TestGoodbyeMarksTheResponderGone(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}

	responder.mu.Lock()
	before := responder.gone
	responder.mu.Unlock()
	if before {
		t.Fatal("a fresh responder was already gone")
	}

	local, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("no loopback socket: %v", err)
	}
	defer local.Close()
	responder.mu.Lock()
	responder.conn = local
	responder.mu.Unlock()

	responder.Goodbye()

	responder.mu.Lock()
	after := responder.gone
	responder.mu.Unlock()
	if !after {
		t.Error("Goodbye did not mark the responder gone, so the read loop keeps answering")
	}
}

func TestAGoodbyeIsTheAnnouncementWithTheTTLsChanged(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43)
	responder.mu.Unlock()

	live := responder.records(ttlShared)
	dead := responder.goodbyeRecords()
	if len(live) != len(dead) {
		t.Fatalf("goodbye is %d bytes against %d live; it withdraws a different set", len(dead), len(live))
	}
	if bytes.Equal(live, dead) {
		t.Fatal("the goodbye is byte-identical to the announcement")
	}
}

func TestReplyToDeclinesForEachReason(t *testing.T) {
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	_, subnet, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	fresh := func() *Responder {
		r := &Responder{Instance: "kitchen", Services: testServices()}
		r.ip, r.subnet = net.IPv4(192, 0, 2, 43), subnet
		return r
	}
	onLink := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 9), Port: 5353}
	offLink := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 9), Port: 5353}
	ask := query(1, []string{testService}, dnsTypePTR, dnsClassIN)

	if dst, _, ok := fresh().replyTo(onLink, group, ask); !ok || !dst.IP.Equal(group.IP) {
		t.Fatalf("an ordinary query was not answered to the group: dst=%v ok=%v", dst, ok)
	}
	if _, _, ok := fresh().replyTo(offLink, group, ask); ok {
		t.Error("answered a querier from another subnet")
	}
	if _, _, ok := fresh().replyTo(onLink, group, query(1, []string{"_printer._tcp.local."}, dnsTypePTR, dnsClassIN)); ok {
		t.Error("answered a query for somebody else's service")
	}

	withdrawn := fresh()
	withdrawn.mu.Lock()
	withdrawn.gone = true
	withdrawn.mu.Unlock()
	if _, _, ok := withdrawn.replyTo(onLink, group, ask); ok {
		t.Error("answered after the records were withdrawn")
	}

	limited := fresh()
	if _, _, ok := limited.replyTo(onLink, group, ask); !ok {
		t.Fatal("the first multicast reply was declined")
	}
	if _, _, ok := limited.replyTo(onLink, group, ask); ok {
		t.Error("a second multicast reply inside the window was allowed")
	}
	unicastAsk := query(1, []string{testService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse)
	if dst, _, ok := limited.replyTo(onLink, group, unicastAsk); !ok || !dst.IP.Equal(onLink.IP) {
		t.Errorf("a unicast reply was rate limited with the multicast one: dst=%v ok=%v", dst, ok)
	}
	flood := fresh()
	sent := 0
	for i := 0; i < unicastBurst*3; i++ {
		if _, _, ok := flood.replyTo(onLink, group, unicastAsk); ok {
			sent++
		}
	}
	if sent > unicastBurst {
		t.Errorf("%d unicast replies to %d queries; a spoofed source reflects without bound",
			sent, unicastBurst*3)
	}
}

func TestAFailedMulticastCountsEvenThroughACopiedAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43)
	responder.mu.Unlock()

	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	copied := *group

	responder.noteReplyFailure(&copied, group, errors.New("network is unreachable"))
	if _, rebuild := responder.stale(net.IPv4(192, 0, 2, 43)); !rebuild {
		t.Error("a failed multicast went uncounted because its address was a different pointer")
	}
}

func TestOnlyAFailedMulticastCountsAgainstTheSocket(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43)
	responder.mu.Unlock()

	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	unreachable := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 9), Port: 0}

	responder.noteReplyFailure(unreachable, group, errors.New("sendto: invalid argument"))
	if _, rebuild := responder.stale(net.IPv4(192, 0, 2, 43)); rebuild {
		t.Fatal("a querier that named an unreachable address forced a rebuild")
	}
	responder.noteReplyFailure(group, group, errors.New("network is unreachable"))
	if _, rebuild := responder.stale(net.IPv4(192, 0, 2, 43)); !rebuild {
		t.Fatal("a failed multicast did not rebuild")
	}
}

func TestUnicastRepliesAreRateLimitedToo(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices()}
	for i := 0; i < unicastBurst; i++ {
		if !responder.mayUnicast() {
			t.Fatalf("reply %d of the burst was refused", i+1)
		}
	}
	if responder.mayUnicast() {
		t.Error("a reply past the burst was allowed, so a spoofed source reflects without bound")
	}
	responder.mu.Lock()
	responder.unicastEnd = time.Now().Add(-time.Second)
	responder.mu.Unlock()
	if !responder.mayUnicast() {
		t.Error("the burst did not refill after its window")
	}
}

const secondService = "_sendspin._tcp.local."

func twoServices() []Advert {
	return []Advert{
		{Service: testService, Port: 6053, Records: testRecords()},
		{Service: secondService, Port: 8928, Records: []string{"path=/sendspin", "name=kitchen"}},
	}
}

func TestOneResponderAnswersForEveryServiceItCarries(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices()}
	for _, c := range []struct {
		name  string
		qname string
		qtype uint16
	}{
		{"first service PTR", testService, dnsTypePTR},
		{"second service PTR", secondService, dnsTypePTR},
		{"first instance SRV", "kitchen." + testService, dnsTypeSRV},
		{"second instance SRV", "kitchen." + secondService, dnsTypeSRV},
		{"second instance TXT", "kitchen." + secondService, dnsTypeTXT},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, wanted := responder.wants(query(1, []string{c.qname}, c.qtype, dnsClassIN)); !wanted {
				t.Errorf("did not answer type %d for %s", c.qtype, c.qname)
			}
		})
	}
	if _, _, wanted := responder.wants(query(1, []string{"_other._tcp.local."}, dnsTypePTR, dnsClassIN)); wanted {
		t.Error("answered for a service it does not carry")
	}
}

func TestOneAnnouncementCarriesEveryServiceAndOneAddress(t *testing.T) {
	responder := &Responder{
		Instance: "kitchen",
		Services: twoServices(),
		ip:       net.IPv4(192, 0, 2, 40),
	}
	packet := responder.records(ttlShared)
	if packet == nil {
		t.Fatal("records built nothing")
	}

	var parser dnsmessage.Parser
	if _, err := parser.Start(packet); err != nil {
		t.Fatalf("the announcement does not parse: %v", err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		t.Fatalf("questions do not parse: %v", err)
	}
	services := map[string]bool{}
	for {
		h, err := parser.AnswerHeader()
		if err != nil {
			break
		}
		services[strings.ToLower(h.Name.String())] = true
		if parser.SkipAnswer() != nil {
			break
		}
	}
	for _, want := range []string{testService, secondService} {
		if !services[strings.ToLower(want)] {
			t.Errorf("no PTR answer for %s; got %v", want, services)
		}
	}

	if err := parser.SkipAllAuthorities(); err != nil {
		t.Fatalf("authorities do not parse: %v", err)
	}
	srvPort := map[string]uint16{}
	txt := map[string][]string{}
	addresses := 0
	for {
		h, err := parser.AdditionalHeader()
		if err != nil {
			break
		}
		owner := strings.ToLower(h.Name.String())
		switch h.Type {
		case dnsmessage.TypeSRV:
			srv, err := parser.SRVResource()
			if err != nil {
				t.Fatalf("SRV does not parse: %v", err)
			}
			srvPort[owner] = srv.Port
		case dnsmessage.TypeTXT:
			rec, err := parser.TXTResource()
			if err != nil {
				t.Fatalf("TXT does not parse: %v", err)
			}
			txt[owner] = rec.TXT
		case dnsmessage.TypeA:
			if _, err := parser.AResource(); err != nil {
				t.Fatalf("A does not parse: %v", err)
			}
			addresses++
		default:
			if parser.SkipAdditional() != nil {
				return
			}
		}
	}

	for _, a := range twoServices() {
		instance := strings.ToLower(responder.instanceFor(a.Service))
		if got := srvPort[instance]; got != a.Port {
			t.Errorf("%s has SRV port %d, want %d", instance, got, a.Port)
		}
		if got := txt[instance]; !slices.Equal(got, a.Records) {
			t.Errorf("%s carries TXT %v, want %v", instance, got, a.Records)
		}
	}
	if addresses != 1 {
		t.Errorf("%d address records; the services share one host, so want 1", addresses)
	}
}

func TestAResponderNeedsAServiceToAdvertise(t *testing.T) {
	for _, c := range []struct {
		name     string
		services []Advert
	}{
		{"none at all", nil},
		{"no records", []Advert{{Service: secondService, Port: 8928}}},
		{"no port", []Advert{{Service: secondService, Records: []string{"path=/x"}}}},
		{"no service", []Advert{{Port: 8928, Records: []string{"path=/x"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Responder{Instance: "kitchen", Services: c.services}
			if err := r.checkRecords(); !errors.Is(err, errNoRecords) {
				t.Errorf("err = %v, want %v", err, errNoRecords)
			}
		})
	}
}

func queryWithKnownAnswers(questions []string, qtype uint16, known [][2]string, ttl uint32) []byte {
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(questions)))
	binary.BigEndian.PutUint16(packet[6:8], uint16(len(known)))
	for _, q := range questions {
		packet = append(packet, encodeName(q)...)
		head := make([]byte, 4)
		binary.BigEndian.PutUint16(head[0:2], qtype)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		packet = append(packet, head...)
	}
	for _, k := range known {
		rdata := encodeName(k[1])
		packet = append(packet, encodeName(k[0])...)
		head := make([]byte, 10)
		binary.BigEndian.PutUint16(head[0:2], dnsTypePTR)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		binary.BigEndian.PutUint32(head[4:8], ttl)
		binary.BigEndian.PutUint16(head[8:10], uint16(len(rdata)))
		packet = append(packet, head...)
		packet = append(packet, rdata...)
	}
	return packet
}

func services(adverts []served) []string {
	out := make([]string, 0, len(adverts))
	for _, a := range adverts {
		out = append(out, a.Service)
	}
	return out
}

func TestAKnownAnswerSuppressesTheServiceItNamesAndNoOther(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices()}

	for _, c := range []struct {
		name      string
		questions []string
		known     [][2]string
		want      []string
	}{
		{
			"the second service's own known answer suppresses it",
			[]string{secondService},
			[][2]string{{secondService, "kitchen." + secondService}},
			nil,
		},
		{
			"a known answer for the first service does not suppress the second",
			[]string{testService, secondService},
			[][2]string{{testService, "kitchen." + testService}},
			[]string{secondService},
		},
		{
			"a known answer for the second does not suppress the first",
			[]string{testService, secondService},
			[][2]string{{secondService, "kitchen." + secondService}},
			[]string{testService},
		},
		{
			"both known suppresses the whole reply",
			[]string{testService, secondService},
			[][2]string{{testService, "kitchen." + testService}, {secondService, "kitchen." + secondService}},
			nil,
		},
		{
			"neither known answers for both",
			[]string{testService, secondService},
			nil,
			[]string{testService, secondService},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			packet := queryWithKnownAnswers(c.questions, dnsTypePTR, c.known, ttlShared)
			send, _, wanted := responder.wants(packet)
			if wanted != (len(c.want) > 0) {
				t.Fatalf("wanted = %v, want %v", wanted, len(c.want) > 0)
			}
			if got := services(send.adverts); !slices.Equal(got, c.want) {
				t.Errorf("replying for %v, want %v", got, c.want)
			}
		})
	}
}

func TestAReplyCarriesOnlyTheServiceThatWasAskedFor(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}

	send, _, wanted := responder.wants(query(1, []string{secondService}, dnsTypePTR, dnsClassIN))
	if !wanted {
		t.Fatal("did not answer for the second service")
	}
	if got := services(send.adverts); !slices.Equal(got, []string{secondService}) {
		t.Fatalf("replying for %v, want only %v", got, secondService)
	}
	packet := responder.recordsFor(ttlShared, send)
	if bytes.Contains(packet, encodeName(testService)) {
		t.Error("a reply about one service carried the other service's records")
	}
}

func queryWithMixedAnswers(questions []string, qtype uint16, answers []knownRecord) []byte {
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(questions)))
	binary.BigEndian.PutUint16(packet[6:8], uint16(len(answers)))
	for _, q := range questions {
		packet = append(packet, encodeName(q)...)
		head := make([]byte, 4)
		binary.BigEndian.PutUint16(head[0:2], qtype)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		packet = append(packet, head...)
	}
	for _, a := range answers {
		var rdata []byte
		switch a.rrType {
		case dnsTypePTR:
			rdata = encodeName(a.alias)
		case dnsTypeA:
			rdata = []byte{192, 0, 2, 41}
		case dnsTypeTXT:
			rdata = append([]byte{byte(len(a.alias))}, []byte(a.alias)...)
		}
		packet = append(packet, encodeName(a.owner)...)
		head := make([]byte, 10)
		binary.BigEndian.PutUint16(head[0:2], a.rrType)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		binary.BigEndian.PutUint32(head[4:8], a.ttl)
		binary.BigEndian.PutUint16(head[8:10], uint16(len(rdata)))
		packet = append(packet, head...)
		packet = append(packet, rdata...)
	}
	return packet
}

type knownRecord struct {
	owner  string
	rrType uint16
	alias  string
	ttl    uint32
}

func TestAKnownPTRDoesNotSuppressAResolve(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}

	known := []knownRecord{{testService, dnsTypePTR, "kitchen." + testService, ttlShared}}
	for _, qtype := range []uint16{dnsTypeSRV, dnsTypeTXT} {
		packet := queryWithMixedAnswers([]string{"kitchen." + testService}, qtype, known)
		send, _, wanted := responder.wants(packet)
		if !wanted {
			t.Errorf("type %d: a known PTR suppressed the reply to a question it does not answer", qtype)
			continue
		}
		if got := services(send.adverts); !slices.Equal(got, []string{testService}) {
			t.Errorf("type %d: replying for %v, want %v", qtype, got, []string{testService})
		}
	}

	browse := queryWithMixedAnswers([]string{testService}, dnsTypePTR, known)
	if _, _, wanted := responder.wants(browse); wanted {
		t.Error("the same known PTR did not suppress the browse it does answer")
	}
}

func TestKnownAnswersReadPastEveryRecordType(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}

	packet := queryWithMixedAnswers([]string{testService, secondService}, dnsTypePTR, []knownRecord{
		{"someone.local.", dnsTypeA, "", ttlShared},
		{"someone.local.", dnsTypeTXT, "unrelated=1", ttlShared},
		{testService, dnsTypePTR, "kitchen." + testService, ttlShared},
		{secondService, dnsTypePTR, "kitchen." + secondService, ttlShared/2 - 1},
	})

	done := make(chan struct{})
	var send answer
	var wanted bool
	go func() {
		send, _, wanted = responder.wants(packet)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wants did not return; a known answer was read without being consumed")
	}

	if !wanted {
		t.Fatal("suppressed everything though the second service's known answer was stale")
	}
	if got := services(send.adverts); !slices.Equal(got, []string{secondService}) {
		t.Errorf("replying for %v, want %v", got, []string{secondService})
	}
}

func TestAHostQueryAnswersInTheAnswerSection(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}

	send, _, wanted := responder.wants(query(1, []string{responder.hostName()}, dnsTypeA, dnsClassIN))
	if !wanted {
		t.Fatal("did not answer a query for our own host name")
	}
	packet := responder.recordsFor(ttlShared, send)
	if packet == nil {
		t.Fatal("built no packet for a host query")
	}
	if answers := binary.BigEndian.Uint16(packet[6:8]); answers != 1 {
		t.Errorf("%d records in the answer section, want 1: a response with none is not one a peer must read", answers)
	}

	ptr, _, _ := responder.wants(query(1, []string{testService}, dnsTypePTR, dnsClassIN))
	if got := binary.BigEndian.Uint16(responder.recordsFor(ttlShared, ptr)[6:8]); got != 1 {
		t.Errorf("a browse reply carries %d answers, want 1", got)
	}
}

func TestAHostQueryIsNotAnsweredBeforeThereIsAnAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices()}

	if _, _, wanted := responder.wants(query(1, []string{responder.hostName()}, dnsTypeA, dnsClassIN)); wanted {
		t.Error("answered a query for our host name with no address to give")
	}
}

func TestRunRefusesAResponderItCannotAdvertise(t *testing.T) {
	for _, c := range []struct {
		name     string
		instance string
		services []Advert
	}{
		{"no service at all", "kitchen", nil},
		{"a service with no records", "kitchen", []Advert{{Service: secondService, Port: 8928}}},
		{"a service name with no trailing dot", "kitchen",
			[]Advert{{Service: "_x._tcp.local", Port: 8928, Records: []string{"path=/x"}}}},
		{"an instance name too long to pack", strings.Repeat("a", 70), testServices()},
		{"a TXT string over 255 bytes", "kitchen",
			[]Advert{{Service: secondService, Port: 8928, Records: []string{strings.Repeat("z", 256)}}}},
		{"records that are present and empty", "kitchen",
			[]Advert{{Service: secondService, Port: 8928, Records: []string{}}}},
		{"one service carried twice", "kitchen",
			[]Advert{
				{Service: secondService, Port: 8928, Records: []string{"path=/x"}},
				{Service: secondService, Port: 9999, Records: []string{"path=/y"}},
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Responder{Instance: c.instance, Iface: "wlan0", Services: c.services}
			if err := r.checkRecords(); err == nil {
				t.Fatal("checkRecords accepted it")
			}

			done := make(chan struct{})
			go func() { r.Run(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not return, so it did not consult checkRecords")
			}
		})
	}
}

func TestEveryConfigurationThatPassesCanBePacked(t *testing.T) {
	r := &Responder{Instance: "kitchen", Iface: "wlan0", Services: twoServices()}
	if err := r.checkRecords(); err != nil {
		t.Fatalf("checkRecords refused a responder it should accept: %v", err)
	}
	if r.records(ttlShared) == nil {
		t.Error("a responder that passed checkRecords still built nothing")
	}
	if r.goodbyeRecords() == nil {
		t.Error("a responder that passed checkRecords built no goodbye")
	}
}

func TestNothingUnbuildableReachesTheSocket(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback socket: %v", err)
	}
	defer conn.Close()

	r := &Responder{Instance: "kitchen", Services: testServices()}
	if err := r.writeTo(conn, nil, conn.LocalAddr().(*net.UDPAddr)); !errors.Is(err, errNothingToSend) {
		t.Errorf("writeTo(nil) = %v, want %v: an empty datagram is a silent no-op on the wire",
			err, errNothingToSend)
	}
	if err := r.writeTo(conn, []byte{}, conn.LocalAddr().(*net.UDPAddr)); !errors.Is(err, errNothingToSend) {
		t.Errorf("writeTo(empty) = %v, want %v", err, errNothingToSend)
	}
}

func TestAKnownAnswerAtExactlyHalfTheTTLStillSuppresses(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices(), ip: net.IPv4(192, 0, 2, 40)}

	for _, c := range []struct {
		name     string
		ttl      uint32
		suppress bool
	}{
		{"one over half", ttlShared/2 + 1, true},
		{"exactly half", ttlShared / 2, true},
		{"one under half", ttlShared/2 - 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			packet := queryWithKnownAnswer(testService, responder.instanceFor(testService), c.ttl)
			_, _, wanted := responder.wants(packet)
			if wanted == c.suppress {
				t.Errorf("ttl %d: wanted = %v, want %v", c.ttl, wanted, !c.suppress)
			}
		})
	}
}

func TestAKnownAnswerMatchesWhateverItsCase(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices(), ip: net.IPv4(192, 0, 2, 40)}

	packet := queryWithKnownAnswerOwnedBy(testService, strings.ToUpper(testService),
		strings.ToUpper(responder.instanceFor(testService)), ttlShared)
	if _, _, wanted := responder.wants(packet); wanted {
		t.Error("a known answer in upper case did not suppress the reply")
	}
}

func TestAnAnyQueryForTheHostIsAnswered(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices(), ip: net.IPv4(192, 0, 2, 40)}

	for _, qtype := range []uint16{dnsTypeA, dnsTypeANY} {
		send, _, wanted := responder.wants(query(1, []string{responder.hostName()}, qtype, dnsClassIN))
		if !wanted {
			t.Errorf("type %d for our host name went unanswered", qtype)
			continue
		}
		if !send.host {
			t.Errorf("type %d matched the host without asking for its address", qtype)
		}
	}
}

func TestTheReplyIsMulticastIfAnyQuestionAskedForOne(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: testServices(), ip: net.IPv4(192, 0, 2, 40)}

	both := func(first, second uint16) []byte {
		packet := make([]byte, 12)
		binary.BigEndian.PutUint16(packet[4:6], 2)
		for _, q := range []struct {
			name  string
			class uint16
		}{
			{responder.hostName(), first},
			{testService, second},
		} {
			packet = append(packet, encodeName(q.name)...)
			head := make([]byte, 4)
			binary.BigEndian.PutUint16(head[0:2], dnsTypeANY)
			binary.BigEndian.PutUint16(head[2:4], q.class)
			packet = append(packet, head...)
		}
		return packet
	}

	if _, unicast, _ := responder.wants(both(dnsClassIN|dnsUnicastResponse, dnsClassIN)); unicast {
		t.Error("one question asked for a multicast reply, so the reply must not be unicast")
	}
	if _, unicast, _ := responder.wants(both(dnsClassIN, dnsClassIN|dnsUnicastResponse)); unicast {
		t.Error("question order decided the reply mode; a multicast question must win")
	}
	if _, unicast, _ := responder.wants(both(dnsClassIN|dnsUnicastResponse, dnsClassIN|dnsUnicastResponse)); !unicast {
		t.Error("every question asked for unicast, so the reply should be unicast")
	}
}

func TestInterfaceIPv4RefusesANameThatIsNotThere(t *testing.T) {
	if _, _, err := interfaceIPv4("definitely-not-an-interface"); err == nil {
		t.Error("named an interface that does not exist and got an address")
	}
}

func sectionsOf(t *testing.T, packet []byte) (answers, additionals []parsedRecord) {
	t.Helper()
	var parser dnsmessage.Parser
	if _, err := parser.Start(packet); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		t.Fatalf("questions: %v", err)
	}
	read := func(all func() ([]dnsmessage.Resource, error)) []parsedRecord {
		got, err := all()
		if err != nil && err != dnsmessage.ErrSectionDone {
			t.Fatalf("section: %v", err)
		}
		var out []parsedRecord
		for _, resource := range got {
			out = append(out, parsedRecord{
				name:   resource.Header.Name.String(),
				rrType: uint16(resource.Header.Type),
				ttl:    resource.Header.TTL,
			})
		}
		return out
	}
	answers = read(parser.AllAnswers)
	if _, err := parser.AllAuthorities(); err != nil && err != dnsmessage.ErrSectionDone {
		t.Fatalf("authorities: %v", err)
	}
	additionals = read(parser.AllAdditionals)
	return answers, additionals
}

func has(records []parsedRecord, name string, rrType uint16) bool {
	for _, r := range records {
		if strings.EqualFold(r.name, name) && r.rrType == rrType {
			return true
		}
	}
	return false
}

func TestAResolveAnswersWithTheRecordsItWasAskedFor(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}
	instance := responder.instanceFor(testService)

	for _, qtype := range []uint16{dnsTypeSRV, dnsTypeTXT} {
		send, _, wanted := responder.wants(query(1, []string{instance}, qtype, dnsClassIN))
		if !wanted {
			t.Fatalf("type %d for %s went unanswered", qtype, instance)
		}
		answers, additionals := sectionsOf(t, responder.recordsFor(ttlShared, send))

		if !has(answers, instance, dnsTypeSRV) || !has(answers, instance, dnsTypeTXT) {
			t.Errorf("type %d: the records asked for are not in the answer section: %v", qtype, answers)
		}
		if has(answers, testService, dnsTypePTR) || has(additionals, testService, dnsTypePTR) {
			t.Errorf("type %d: a resolve carried a PTR nothing asked about", qtype)
		}
		if !has(additionals, responder.hostName(), dnsTypeA) {
			t.Errorf("type %d: no address for %s, so the target cannot be reached",
				qtype, responder.hostName())
		}
	}
}

func TestABrowseKeepsItsRecordsWhereTheyWere(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}
	instance := responder.instanceFor(testService)

	send, _, wanted := responder.wants(query(1, []string{testService}, dnsTypePTR, dnsClassIN))
	if !wanted {
		t.Fatal("a browse went unanswered")
	}
	answers, additionals := sectionsOf(t, responder.recordsFor(ttlShared, send))

	if !has(answers, testService, dnsTypePTR) {
		t.Errorf("the PTR that answers the browse is not in the answer section: %v", answers)
	}
	if !has(additionals, instance, dnsTypeSRV) || !has(additionals, instance, dnsTypeTXT) {
		t.Errorf("SRV and TXT left the additional section: %v", additionals)
	}
	if !has(additionals, responder.hostName(), dnsTypeA) {
		t.Error("a browse reply carries no address")
	}
}

func TestAKnownPTRIsNotSentBackAlongsideAResolve(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Services: twoServices(), ip: net.IPv4(192, 0, 2, 40)}
	instance := responder.instanceFor(testService)

	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], 2)
	binary.BigEndian.PutUint16(packet[6:8], 1)
	for _, q := range []struct {
		name  string
		qtype uint16
	}{{testService, dnsTypePTR}, {instance, dnsTypeSRV}} {
		packet = append(packet, encodeName(q.name)...)
		head := make([]byte, 4)
		binary.BigEndian.PutUint16(head[0:2], q.qtype)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		packet = append(packet, head...)
	}
	rdata := encodeName(instance)
	packet = append(packet, encodeName(testService)...)
	head := make([]byte, 10)
	binary.BigEndian.PutUint16(head[0:2], dnsTypePTR)
	binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
	binary.BigEndian.PutUint32(head[4:8], ttlShared)
	binary.BigEndian.PutUint16(head[8:10], uint16(len(rdata)))
	packet = append(packet, head...)
	packet = append(packet, rdata...)

	send, _, wanted := responder.wants(packet)
	if !wanted {
		t.Fatal("the resolve went unanswered because the browse was already known")
	}
	answers, additionals := sectionsOf(t, responder.recordsFor(ttlShared, send))

	if has(answers, testService, dnsTypePTR) || has(additionals, testService, dnsTypePTR) {
		t.Error("sent back the PTR the querier listed as known, which RFC 6762 7.1 forbids")
	}
	if !has(answers, instance, dnsTypeSRV) || !has(answers, instance, dnsTypeTXT) {
		t.Errorf("the resolve lost its records: %v", answers)
	}
}

func TestTheFirstSendFailureIsTheOneReported(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Services: testServices()}

	first := errors.New("network is unreachable")
	responder.noteSendFailure(first)
	responder.noteSendFailure(errors.New("something later and less useful"))

	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 40)
	responder.mu.Unlock()
	reason, rebuild := responder.stale(net.IPv4(192, 0, 2, 40))
	if !rebuild {
		t.Fatal("a send failure did not ask for a rebuild")
	}
	if !strings.Contains(reason, first.Error()) {
		t.Errorf("reported %q, want the first failure %q", reason, first)
	}
}

func TestAnUnusableNameIsNamedInTheError(t *testing.T) {
	for _, c := range []struct {
		name     string
		instance string
		services []Advert
		says     string
	}{
		{"no trailing dot", "kitchen",
			[]Advert{{Service: "_x._tcp.local", Port: 1, Records: []string{"a=b"}}},
			"does not end in a dot"},
		{"a label over 63 bytes", strings.Repeat("a", 70), testServices(), "-byte label"},
		{"an empty label", "kitchen",
			[]Advert{{Service: "_x..local.", Port: 1, Records: []string{"a=b"}}},
			"empty label"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Responder{Instance: c.instance, Services: c.services}
			err := r.checkRecords()
			if err == nil {
				t.Fatal("checkRecords accepted it")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("err = %q, want it to say %q rather than fall through to the dry run",
					err, c.says)
			}
		})
	}
}
