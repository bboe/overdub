package main

import (
	"os"
	"strings"
	"testing"
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
		"\"overdub-$GITHUB_REF_NAME.tar.gz\" SHA256SUMS SHA256SUMS.sig",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Errorf(".github/workflows/ci.yml does not say %q", want)
		}
	}
}
