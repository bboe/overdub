package sendspin

import (
	"fmt"
	"log"
	"strings"
	"testing"
)

func TestAPeerSuppliedNumberCannotStretchALogLine(t *testing.T) {
	var out lockedLog
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	ln := listenLocal(t)
	c := testClient(t)
	go func() { _ = c.Serve(ln) }()

	peer := dialLocal(t, ln)
	peer.upgrade("/sendspin")
	peer.readRaw(typeClientInit)
	peer.writeText([]byte(fmt.Sprintf(
		`{"type":"server/init","payload":{"server_id":"x","version":%s}}`,
		strings.Repeat("9", 1900))))
	waitFor(t, "the handshake failure to be logged, or this proves nothing", func() bool {
		return strings.Contains(out.String(), "server/init")
	})

	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 700 {
			t.Errorf("a peer wrote a %d-byte log line with one number", len(line))
		}
	}
}
