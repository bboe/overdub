package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bboe/overdub/internal/update"
)

func TestTheReleaseJobSignsWhatTheDaemonChecks(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"environment: release",
		"openssl pkeyutl -sign -rawin -inkey \"$key\" -in SHA256SUMS -out SHA256SUMS.sig",
		"openssl pkeyutl -verify -rawin -pubin -inkey deploy/release-signing.pub",
		"-in SHA256SUMS -sigfile SHA256SUMS.sig",
		"- run: deploy/package.sh \"$GITHUB_REF_NAME\"",
		"- run: deploy/package.sh v0.0.0-ci",
		"--generate-notes --notes-file deploy/fireos5-decoy/notes.md",
		"\"overdub-$GITHUB_REF_NAME-fireos6.tar.gz\" \"overdub-$GITHUB_REF_NAME.tar.gz\" release.json SHA256SUMS SHA256SUMS.sig",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Errorf(".github/workflows/ci.yml does not say %q", want)
		}
	}
	script, err := os.ReadFile("deploy/package.sh")
	if err != nil {
		t.Fatal(err)
	}
	if want := `sha256sum "$dir-fireos6.tar.gz" "$dir.tar.gz" release.json > SHA256SUMS`; !strings.Contains(string(script), want) {
		t.Errorf("deploy/package.sh does not say %q", want)
	}
}

func TestTheFireOS5DecoyIsRefusedForItsBootScript(t *testing.T) {
	script, err := os.ReadFile("deploy/package.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cp deploy/fireos5-decoy/README.md "$decoy/$dir/README.md"`,
		`cp deploy/fireos5-decoy/overdub.sh "$decoy/$dir/deploy/overdub.sh"`,
		`cp deploy/fireos5-decoy/placeholder "$decoy/$dir/build/overdub"`,
		`cp deploy/fireos5-decoy/placeholder "$decoy/$dir/deploy/mapdump/mapdump.jar"`,
		`gzip -n > "$dir.tar.gz"`,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("deploy/package.sh does not say %q", want)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			t.Errorf("%s is empty, so v0.3.0 refuses the decoy as missing it, with words that do not help", path)
		}
		return b
	}
	read("deploy/fireos5-decoy/README.md")
	read("deploy/fireos5-decoy/notes.md")
	read("deploy/fireos5-decoy/placeholder")
	decoy, real := read("deploy/fireos5-decoy/overdub.sh"), read("deploy/overdub.sh")
	nameLine := regexp.MustCompile(`(?m)^NAME=$`)
	if n := len(nameLine.FindAllIndex(decoy, -1)); n != 1 {
		t.Fatalf("the decoy's boot script has %d NAME= lines; v0.3.0 needs 1 to reach the boot script check", n)
	}
	named := func(b []byte) []byte { return nameLine.ReplaceAll(b, []byte("NAME=kitchen")) }
	if bytes.Equal(named(decoy), named(real)) {
		t.Error("the decoy's boot script is the real one, so v0.3.0 would install the placeholder")
	}
}

func TestTheManifestLetsThisBuildsDotsInstallIt(t *testing.T) {
	body, err := os.ReadFile("deploy/release.json")
	if err != nil {
		t.Fatal(err)
	}
	var m update.Manifest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("deploy/release.json: %v", err)
	}
	for _, board := range []string{"biscuit", "donut", "radar"} {
		if why := m.Refuses(board, cmp.Or(m.From, "v0.0.0")); why != "" {
			t.Errorf("a %s on this build would refuse its own release: %s", board, why)
		}
	}
	if m.Refusal == "" {
		t.Error("deploy/release.json has no refusal, so a Dot it does not fit is told nothing of its own")
	}
}
