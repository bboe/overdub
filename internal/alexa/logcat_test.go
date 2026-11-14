package alexa

import (
	"errors"
	"testing"
)

func TestALogcatThatWillNotStartIsSaidOnce(t *testing.T) {
	boom := errors.New("fork/exec logcat: no such file or directory")

	say, said := shouldSay(false, 0, boom)
	if !say {
		t.Fatal("the first failure was not reported at all")
	}
	for i := 0; i < 100; i++ {
		say, said = shouldSay(said, 0, boom)
		if say {
			t.Fatalf("failure %d was reported again; at %v apiece that is a line a minute "+
				"for ever, into a log on /data that is truncated at boot", i+2, watchRetry)
		}
	}

	if say, _ = shouldSay(said, watchRetry*3, boom); !say {
		t.Error("a tail that ran for a while and then failed said nothing: that is a new " +
			"fault rather than the one already reported")
	}
	if say, _ = shouldSay(false, 0, nil); say {
		t.Error("a tail that ended without error was reported as a failure")
	}
}
