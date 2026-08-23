package esphome

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestRecordsFollowTheCurrentAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Iface: "wlan0", Port: 6053}

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
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}

	without := binary.BigEndian.Uint16(responder.records(ttlShared)[10:12])
	responder.ip = net.IPv4(192, 0, 2, 43)
	with := binary.BigEndian.Uint16(responder.records(ttlShared)[10:12])

	if with != without+1 {
		t.Errorf("additional records: %d without an address, %d with; want one more", without, with)
	}
}

func TestTXTFriendlyNameMatchesTheName(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}

	var got string
	for _, e := range responder.txt() {
		if strings.HasPrefix(e, "friendly_name=") {
			got = strings.TrimPrefix(e, "friendly_name=")
		}
	}
	if got != "kitchen" {
		t.Errorf("friendly_name = %q, want %q", got, "kitchen")
	}
}

func TestTXTCarriesTheMAC(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}

	var got string
	for _, e := range responder.txt() {
		if strings.HasPrefix(e, "mac=") {
			got = strings.TrimPrefix(e, "mac=")
		}
	}
	if got != "00005e00532a" {
		t.Errorf("mac = %q, want %q", got, "00005e00532a")
	}
}

// The tests build queries as raw bytes on purpose: a packet is what a peer
// sends, and constructing one with the same library that parses it would hide
// a disagreement between them.
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
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
	tests := []struct {
		name  string
		qname string
		qtype uint16
		want  bool
	}{
		{"service PTR", esphomeService, dnsTypePTR, true},
		{"service ANY", esphomeService, dnsTypeANY, true},
		{"service A is not ours to answer", esphomeService, dnsTypeA, false},
		{"meta-query PTR", dnssdMeta, dnsTypePTR, false},
		{"meta-query SRV", dnssdMeta, dnsTypeSRV, false},
		{"instance SRV", "kitchen." + esphomeService, dnsTypeSRV, true},
		{"instance TXT", "kitchen." + esphomeService, dnsTypeTXT, true},
		{"host A", "kitchen.local.", dnsTypeA, true},
		{"host SRV", "kitchen.local.", dnsTypeSRV, false},
		{"another device's host", "bedroom.local.", dnsTypeA, false},
		{"case is ignored", "KITCHEN.LOCAL.", dnsTypeA, true},
	}
	for _, tt := range tests {
		_, got := responder.wants(query(1, []string{tt.qname}, tt.qtype, dnsClassIN))
		if got != tt.want {
			t.Errorf("%s: wants(%q, %d) = %v, want %v", tt.name, tt.qname, tt.qtype, got, tt.want)
		}
	}
}

func TestWantsReportsTheUnicastBit(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Port: 6053}

	unicast, wanted := responder.wants(query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse))
	if !wanted || !unicast {
		t.Errorf("unicast bit set: got unicast=%v wanted=%v, want true true", unicast, wanted)
	}
	unicast, wanted = responder.wants(query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN))
	if !wanted || unicast {
		t.Errorf("unicast bit clear: got unicast=%v wanted=%v, want false true", unicast, wanted)
	}
}

func TestWantsScansEveryQuestion(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Port: 6053}
	packet := query(2, []string{"unrelated.local.", esphomeService}, dnsTypePTR, dnsClassIN)
	if _, wanted := responder.wants(packet); !wanted {
		t.Error("a match in the second question was missed")
	}
}

func TestWantsRejectsMalformed(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Port: 6053}
	good := query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN)

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
		if _, wanted := responder.wants(tt.packet); wanted {
			t.Errorf("%s: wants() said yes to a malformed packet", tt.name)
		}
	}
}

// The shapes the wire format forbids, fed through the only door a peer has.
// The parser behind it is x/net/dns/dnsmessage rather than something here, so
// what is worth pinning is no longer how a name is read but that none of these
// draws a reply or takes the daemon down: a panic here is a restart with the
// button ungrabbed, and one peer repeating one packet is a reboot loop.
func TestWantsSurvivesWhatTheWireFormatForbids(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}

	header := func(questions, answers int) []byte {
		packet := make([]byte, 12)
		binary.BigEndian.PutUint16(packet[4:6], uint16(questions))
		binary.BigEndian.PutUint16(packet[6:8], uint16(answers))
		return packet
	}
	question := append(encodeName(esphomeService), 0, dnsTypePTR, 0, dnsClassIN)

	tests := []struct {
		name string
		// A question this daemon cannot read is not one it answers. A question
		// it can read is answered even when what follows is rubbish, which is
		// no worse than the plain query it already answers: RFC 6762 asks for
		// suppression when a known answer is present, not when one is claimed.
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
			if _, wanted := responder.wants(tt.packet); wanted && !tt.mayReply {
				t.Errorf("%s: answered a query the wire format forbids", tt.name)
			}
		}()
	}
}

func TestWantsIgnoresResponses(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Port: 6053}
	packet := query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN)
	packet[2] |= 0x80 // QR
	if _, wanted := responder.wants(packet); wanted {
		t.Error("answered a response rather than a query")
	}
}

func TestWantsFollowsACompressedQuestionName(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Port: 6053}

	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[4:6], 2)
	packet = append(packet, encodeName(esphomeService)...)
	packet = append(packet, 0, dnsTypeA, 0, dnsClassIN) // not a type we answer
	packet = append(packet, 0xc0, 12)                   // the same name, by pointer
	packet = append(packet, 0, dnsTypePTR, 0, dnsClassIN)

	if _, wanted := responder.wants(packet); !wanted {
		t.Error("a compressed question name was not matched")
	}
}

func TestOnLinkGatesTheSubnet(t *testing.T) {
	_, subnet, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	responder := &Responder{Instance: "kitchen", Port: 6053}
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
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Port: 6053}
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
	// The error is carried here rather than logged where it happened, so it has
	// to survive the trip or the diagnosis is lost with the line that had it.
	if !strings.Contains(reason, "network is unreachable") {
		t.Errorf("reason = %q, want it to carry the error", reason)
	}
}

func TestStaleClearsTheSendFlag(t *testing.T) {
	address := net.IPv4(192, 0, 2, 44)
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Port: 6053}
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
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Port: 6053}
	responder.ip = net.IPv4(192, 0, 2, 44)

	reason, rebuild := responder.stale(net.IPv4(192, 0, 2, 48))
	if !rebuild {
		t.Fatal("a changed address did not rebuild")
	}
	if !strings.Contains(reason, "changed to") {
		t.Errorf("reason = %q, want it to name the change", reason)
	}
}

// walkRecords yields the type and TTL of every resource record in a response.
type parsedRecord struct {
	name   string
	rrType uint16
	class  uint16
	ttl    uint32
	port   uint16 // SRV only
	points string // the name in the rdata of a PTR or SRV
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

// Which name owns which record is the whole of DNS-SD, and getting it wrong is
// invisible to everything that only reads types and TTLs: the browser follows
// PTR to the instance, SRV from the instance to the host, and A at the host. A
// record hung off the wrong name leaves Home Assistant resolving forever.
func TestEveryRecordIsOwnedByTheNameThatShouldOwnIt(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
	responder.mu.Lock()
	responder.ip = net.IPv4(192, 0, 2, 43).To4()
	responder.mu.Unlock()

	want := map[uint16]struct{ name, points string }{
		dnsTypePTR: {esphomeService, responder.serviceName()},
		dnsTypeSRV: {responder.serviceName(), responder.hostName()},
		dnsTypeTXT: {responder.serviceName(), ""},
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

// The names and numbers on the wire are Home Assistant's, not ours, so a test
// that compares them against the constants they came from asserts nothing. A
// typo in any of these breaks discovery completely and says nothing about why.
func TestTheWireConstantsAreTheOnesHomeAssistantUses(t *testing.T) {
	if esphomeService != "_esphomelib._tcp.local." {
		t.Errorf("esphomeService = %q; Home Assistant browses for _esphomelib._tcp.local.", esphomeService)
	}
	if dnssdMeta != "_services._dns-sd._udp.local." {
		t.Errorf("dnssdMeta = %q, want the RFC 6763 meta-query name", dnssdMeta)
	}
	if mdnsPort != 5353 {
		t.Errorf("mdnsPort = %d, want 5353", mdnsPort)
	}
	// python-zeroconf drops a byte-identical packet seen inside one second, so
	// a goodbye sent twice any faster than this is read once.
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
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
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
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
	responder.ip = net.IPv4(192, 0, 2, 43)

	for _, r := range walkRecords(t, responder.records(ttlShared)) {
		switch r.rrType {
		case dnsTypeSRV, dnsTypeA:
			// Both name a host, and RFC 6762 section 10 asks for the short TTL there.
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

// A query carrying our own PTR as a known answer, the way python-zeroconf's
// browser sends it on every repeat.
// The question and the known answer carry separate owner names, because
// suppression turns on both: the answer has to be a PTR for the service that
// was asked about, and not merely a PTR whose target happens to be ours.
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

// A browse that asks for several service types at once puts our question
// somewhere in the middle. The answer section starts after all of them, so a
// reader that walked only as far as the match reads the questions it has not
// reached yet as records, and the suppression stops happening.
func TestAKnownAnswerSuppressesTheReplyWhereverTheQuestionSits(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
	others := []string{"_printer._tcp.local.", "_airplay._tcp.local."}

	for position := 0; position <= len(others); position++ {
		names := append(append([]string{}, others[:position]...), esphomeService)
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
		rdata := encodeName(responder.serviceName())
		packet = append(packet, encodeName(esphomeService)...)
		head := make([]byte, 10)
		binary.BigEndian.PutUint16(head[0:2], dnsTypePTR)
		binary.BigEndian.PutUint16(head[2:4], dnsClassIN)
		binary.BigEndian.PutUint32(head[4:8], ttlShared)
		binary.BigEndian.PutUint16(head[8:10], uint16(len(rdata)))
		packet = append(packet, head...)
		packet = append(packet, rdata...)

		if _, wanted := responder.wants(packet); wanted {
			t.Errorf("replied to a query carrying our own PTR, with our question at position %d of %d",
				position+1, len(names))
		}
	}
}

// Home Assistant browses about ninety service types in one packet, and where
// _esphomelib sits in that list is not ours to choose: it is alphabetical, and
// every integration added above it moves ours further down. A label budget that
// an ordinary browse can exhaust is a Dot that stops being discoverable, with
// nothing logged to say why.
func TestARealBrowseIsAnsweredWhereverOurQuestionSits(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
	const types = 90

	for ours := 0; ours < types; ours++ {
		packet := make([]byte, 12)
		binary.BigEndian.PutUint16(packet[4:6], types)
		for i := 0; i < types; i++ {
			name := fmt.Sprintf("_svc%03d._tcp.local.", i)
			if i == ours {
				name = esphomeService
			}
			packet = append(packet, encodeName(name)...)
			packet = append(packet, 0, 0, 0, 0)
			binary.BigEndian.PutUint16(packet[len(packet)-4:len(packet)-2], dnsTypePTR)
			binary.BigEndian.PutUint16(packet[len(packet)-2:], dnsClassIN)
		}
		if _, wanted := responder.wants(packet); !wanted {
			t.Fatalf("refused a %d-question browse with our question at index %d", types, ours)
		}
	}
}

// The class word and the header are the two things python-zeroconf reads before
// it reads anything else, and getting either wrong is silent. A cleared QR bit
// makes the announcement a query, which its listener never hands to the record
// manager: discovery simply never happens. Cache-flush on the shared PTR makes
// that rrset unique, and its record manager then expires every *other* ESPHome
// device on the segment each time this Dot announces.
func TestTheHeaderAndTheClassesAreWhatZeroconfReads(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
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
		if record.rrType == dnsTypeSRV && record.port != responder.Port {
			t.Errorf("SRV port = %d, want %d", record.port, responder.Port)
		}
		seen[record.rrType] = true
	}
	for rrType := range want {
		if !seen[rrType] {
			t.Errorf("no record of type %d in the announcement", rrType)
		}
	}
}

// RFC 6762 section 7.1. Without this every browser refresh on the segment draws
// a full multicast reply, which is what makes the flood an everyday event
// rather than an attack.
func TestAKnownAnswerSuppressesTheReply(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}

	full := queryWithKnownAnswer(esphomeService, responder.serviceName(), ttlShared)
	if _, wanted := responder.wants(full); wanted {
		t.Error("answered a query that already carried our PTR at the full TTL")
	}
	// Half the TTL is the floor, so just under it must still be answered.
	stale := queryWithKnownAnswer(esphomeService, responder.serviceName(), ttlShared/2-1)
	if _, wanted := responder.wants(stale); !wanted {
		t.Error("suppressed on a known answer that had aged past half its TTL")
	}
	// Somebody else's PTR is not ours.
	other := queryWithKnownAnswer(esphomeService, "elsewhere."+esphomeService, ttlShared)
	if _, wanted := responder.wants(other); !wanted {
		t.Error("suppressed on another device's known answer")
	}
	// Nor is our own instance name hung off a service we do not answer for.
	elsewhere := queryWithKnownAnswerOwnedBy(esphomeService, "_printer._tcp.local.", responder.serviceName(), ttlShared)
	if _, wanted := responder.wants(elsewhere); !wanted {
		t.Error("suppressed on a known answer owned by a service that is not ours")
	}
}

// RFC 6762 section 6, and the only unbounded thing a peer can ask this daemon
// for: a 40-byte query draws 328 bytes back at the shortest name this allows
// and 700 at the longest, aimed at the whole segment.
func TestTheMulticastRateLimitIsOneASecond(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}

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

// Home Assistant reads this key to decide whether to probe in plaintext first,
// and a real device with a key publishes it.
func TestTXTAnnouncesTheEncryption(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
	want := "api_encryption=" + noiseCipherName
	for _, entry := range responder.txt() {
		if entry == want {
			return
		}
	}
	t.Errorf("txt() = %q, want it to carry %q", responder.txt(), want)
}

// The flag belongs to the socket that failed. Carried into the next cycle it
// tears down a replacement that is working, and the teardown reads as
// deliberate so nothing says why.
func TestServeClearsTheFlagsOfThePreviousCycle(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Port: 6053}
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

// Goodbye is the daemon on its way out, so it has to stop the responder
// answering as well as withdraw: a query already in the socket buffer would
// otherwise be answered with a 4500-second TTL and re-advertise what was just
// retired.
func TestGoodbyeMarksTheResponderGone(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Iface: "wlan0", Port: 6053}

	responder.mu.Lock()
	before := responder.gone
	responder.mu.Unlock()
	if before {
		t.Fatal("a fresh responder was already gone")
	}

	// A socket of the test's own. Left without one, Goodbye opens a real
	// multicast socket and withdraws kitchen._esphomelib._tcp.local. from every
	// cache on whatever network the machine running the tests is attached to.
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

// The withdrawal has to be the same record set, or it retires something other
// than what was advertised. That the TTLs are zero is TestGoodbyeRetiresEveryRecord.
func TestAGoodbyeIsTheAnnouncementWithTheTTLsChanged(t *testing.T) {
	responder := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
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

// The four reasons the read loop declines to answer, which used to live inside
// serve() where a socket was needed to reach them. Each is deleted by a one-line
// mutation, and each one deleted is a different way for the responder to answer
// something it should not.
func TestReplyToDeclinesForEachReason(t *testing.T) {
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	_, subnet, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	fresh := func() *Responder {
		r := &Responder{Instance: "kitchen", MAC: "00:00:5E:00:53:2A", Port: 6053}
		r.ip, r.subnet = net.IPv4(192, 0, 2, 43), subnet
		return r
	}
	onLink := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 9), Port: 5353}
	offLink := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 9), Port: 5353}
	ask := query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN)

	if dst, ok := fresh().replyTo(onLink, group, ask); !ok || !dst.IP.Equal(group.IP) {
		t.Fatalf("an ordinary query was not answered to the group: dst=%v ok=%v", dst, ok)
	}
	if _, ok := fresh().replyTo(offLink, group, ask); ok {
		t.Error("answered a querier from another subnet")
	}
	if _, ok := fresh().replyTo(onLink, group, query(1, []string{"_printer._tcp.local."}, dnsTypePTR, dnsClassIN)); ok {
		t.Error("answered a query for somebody else's service")
	}

	withdrawn := fresh()
	withdrawn.mu.Lock()
	withdrawn.gone = true
	withdrawn.mu.Unlock()
	if _, ok := withdrawn.replyTo(onLink, group, ask); ok {
		t.Error("answered after the records were withdrawn")
	}

	limited := fresh()
	if _, ok := limited.replyTo(onLink, group, ask); !ok {
		t.Fatal("the first multicast reply was declined")
	}
	if _, ok := limited.replyTo(onLink, group, ask); ok {
		t.Error("a second multicast reply inside the window was allowed")
	}
	// A unicast reply goes to the host that asked, so it is not held to the
	// multicast rate limit -- but it has its own, or a spoofed source reflects.
	unicastAsk := query(1, []string{esphomeService}, dnsTypePTR, dnsClassIN|dnsUnicastResponse)
	if dst, ok := limited.replyTo(onLink, group, unicastAsk); !ok || !dst.IP.Equal(onLink.IP) {
		t.Errorf("a unicast reply was rate limited with the multicast one: dst=%v ok=%v", dst, ok)
	}
	flood := fresh()
	sent := 0
	for i := 0; i < unicastBurst*3; i++ {
		if _, ok := flood.replyTo(onLink, group, unicastAsk); ok {
			sent++
		}
	}
	if sent > unicastBurst {
		t.Errorf("%d unicast replies to %d queries; a spoofed source reflects without bound",
			sent, unicastBurst*3)
	}
}

// A querier names where its unicast reply goes, and can name somewhere that
// cannot be reached: port zero does it. Counting that as this socket's failure
// handed anyone on the subnet a rebuild every poll, each writing to the log.
// The reply's destination arrives back from replyTo, and what makes it "the
// group" is the address rather than the identity of the pointer carrying it.
func TestAFailedMulticastCountsEvenThroughACopiedAddress(t *testing.T) {
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Port: 6053}
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
	responder := &Responder{Instance: "kitchen", Iface: "wlan0", Port: 6053}
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
	responder := &Responder{Instance: "kitchen", Port: 6053}
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
