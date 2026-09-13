package untrustedlog

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func restoreLog(t *testing.T, buf *lockedBuffer) func() {
	t.Helper()
	was := log.Writer()
	log.SetOutput(buf)
	return func() { log.SetOutput(was) }
}

func TestTheRateLimitCapsWhatOnePeerCanWrite(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	for i := 0; i < 500; i++ {
		l.Printf("test: line %d", i)
	}
	if lines := strings.Count(out.String(), "\n"); lines > Burst+1 {
		t.Errorf("500 peer events wrote %d lines, want at most %d", lines, Burst+1)
	}
}

func TestTheRateLimitHoldsAcrossConcurrentPeers(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.Printf("test: line %d", j)
			}
		}()
	}
	wg.Wait()

	if lines := strings.Count(out.String(), "\n"); lines > Burst+1 {
		t.Errorf("eight peers wrote %d lines in one window, want at most %d",
			lines, Burst+1)
	}
}

func TestLoggingStopsAtItsCeilingForTheRun(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	for i := 0; i < 5060; i++ { // total + Burst*3, as a literal
		l.mu.Lock()
		l.windowEnd = time.Time{}
		l.mu.Unlock()
		l.Printf("test: line %d", i)
	}
	if lines := strings.Count(out.String(), "\n"); lines > total+2 {
		t.Errorf("wrote %d lines, want at most %d: the run has no ceiling",
			lines, total+2)
	}
}

func TestASuppressedCountIsReportedOnceTheWindowTurns(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	for i := 0; i < Burst+5; i++ {
		l.Printf("test: line %d", i)
	}
	if strings.Contains(out.String(), "lines suppressed") {
		t.Error("reported suppressed lines inside the window that suppressed them")
	}

	l.mu.Lock()
	l.windowEnd = time.Time{}
	l.mu.Unlock()
	l.Printf("test: the window has turned")

	if !strings.Contains(out.String(), "test: 5 lines suppressed") {
		t.Errorf("the turned window did not report the 5 dropped lines:\n%s", out.String())
	}
}

func TestTheSubjectNamesWhoIsSpending(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{}
	for i := 0; i < Burst+2; i++ {
		l.Printf("line %d", i)
	}
	l.mu.Lock()
	l.windowEnd = time.Time{}
	l.mu.Unlock()
	l.Printf("after")

	if strings.Contains(out.String(), ": 2 lines suppressed") {
		t.Error("an empty subject still wrote a bare colon into the meta line")
	}
	if !strings.Contains(out.String(), "2 lines suppressed") {
		t.Errorf("no suppressed count at all:\n%s", out.String())
	}
}

func TestCutBoundsWhatAPeerCanPutOnOneLine(t *testing.T) {
	if got := Cut("short"); got != "short" {
		t.Errorf("Cut(%q) = %q, want it unchanged", "short", got)
	}
	long := strings.Repeat("z", maxString*2)
	got := Cut(long)
	if len(got) != maxString+3 {
		t.Errorf("Cut of %d bytes gave %d, want %d", len(long), len(got), maxString+3)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("Cut(%d bytes) = %q, want it to say it was cut", len(long), got)
	}
}

func TestTheCeilingIsAnnouncedExactlyOnce(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	for i := 0; i < 5060; i++ { // total + Burst*3, as a literal
		l.mu.Lock()
		l.windowEnd = time.Time{}
		l.mu.Unlock()
		l.Printf("test: line %d", i)
	}

	const says = "lines this run; nothing a peer does is logged again until a restart"
	if got := strings.Count(out.String(), says); got != 1 {
		t.Errorf("the ceiling was announced %d times, want exactly 1: a peer is owed one line"+
			" saying the log has closed, and no more", got)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("test: %d %s", total, says)) {
		t.Errorf("the announcement does not name the ceiling it reached:\n%s",
			lastLines(out.String(), 3))
	}
}

func TestASuppressedCountIsReportedOnceAndNotAgain(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	for i := 0; i < Burst+5; i++ {
		l.Printf("test: line %d", i)
	}

	turn := func() {
		l.mu.Lock()
		l.windowEnd = time.Time{}
		l.mu.Unlock()
	}

	turn()
	l.Printf("test: the window turned")
	if got := strings.Count(out.String(), "5 lines suppressed"); got != 1 {
		t.Fatalf("the dropped count was reported %d times, want 1", got)
	}

	turn()
	l.Printf("test: and turned again")
	if got := strings.Count(out.String(), "lines suppressed"); got != 1 {
		t.Errorf("a count was reported %d times in all; one that is not cleared is reported"+
			" every window forever, growing the file it is warning about", got)
	}
}

func TestWrittenCountsWhatReachedTheLogAndNothingElse(t *testing.T) {
	l := &Log{Subject: "test"}
	if got := l.Written(); got != 0 {
		t.Errorf("a fresh Log reports %d lines written, want 0", got)
	}

	var out lockedBuffer
	defer restoreLog(t, &out)()
	for i := 0; i < Burst+10; i++ {
		l.Printf("test: line %d", i)
	}
	if got := l.Written(); got != Burst {
		t.Errorf("Written() = %d after %d calls, want %d: the suppressed ones did not"+
			" reach the log and must not be counted as though they had", got, Burst+10, Burst)
	}
}

func TestCutLeavesAnExactlyBoundedStringAlone(t *testing.T) {
	exact := strings.Repeat("z", maxString)
	if got := Cut(exact); got != exact {
		t.Errorf("Cut cut a string that was already exactly %d bytes", maxString)
	}
	over := strings.Repeat("z", maxString+1)
	if got := Cut(over); got == over {
		t.Errorf("Cut left a %d-byte string alone, one past the bound", len(over))
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func TestTheNumbersTheDocsPromiseAreTheNumbersHere(t *testing.T) {
	if Burst != 20 {
		t.Errorf("Burst = %d, want 20: docs/pitfalls.md promises twenty a minute", Burst)
	}
	if total != 5000 {
		t.Errorf("total = %d, want 5000: docs/pitfalls.md promises five thousand a run", total)
	}
	if maxString != 64 {
		t.Errorf("maxString = %d, want 64: docs/pitfalls.md promises peer strings cut at 64",
			maxString)
	}
	if window != time.Minute {
		t.Errorf("window = %v, want a minute", window)
	}
}

func TestWrittenIsSafeToReadWhileLinesAreBeingWritten(t *testing.T) {
	var out lockedBuffer
	defer restoreLog(t, &out)()

	l := &Log{Subject: "test"}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Printf("test: line %d", j)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			_ = l.Written()
		}
	}()
	wg.Wait()
}

func TestALineIsBoundedWhateverTheCallSitePassed(t *testing.T) {
	// Cut is for peer strings the call site knows about. A peer's bytes also arrive
	// inside values the call site does not think of as strings at all: an error from
	// encoding/json carries the offending number literal verbatim.
	var out strings.Builder
	was := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(was)

	var l Log
	l.Printf("boom: %v", errors.New(strings.Repeat("9", 4000)))

	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > maxLine+64 {
			t.Errorf("one line of %d bytes reached the log; a peer sets how much of /data"+
				" it fills", len(line))
		}
	}
	if !strings.Contains(out.String(), "boom:") {
		t.Error("the line was dropped rather than cut")
	}
}
