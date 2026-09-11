package alexa

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClip(t *testing.T) {
	if got := clip([]byte("  short  ")); got != "short" {
		t.Errorf("clip = %q, want %q", got, "short")
	}
	long := strings.Repeat("a", 400)
	got := clip([]byte(long))
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a long body was not marked as clipped: %q", got[len(got)-10:])
	}
	if len(got) > 303 {
		t.Errorf("clip returned %d bytes, want at most 303", len(got))
	}
}

func TestClipStaysValidUTF8(t *testing.T) {
	body := strings.Repeat("é", 400) // two bytes each
	got := clip([]byte(body))
	if !utf8.ValidString(got) {
		t.Error("clip produced invalid UTF-8")
	}
	body3 := strings.Repeat("世", 400) // three bytes each
	if got := clip([]byte(body3)); !utf8.ValidString(got) {
		t.Error("clip produced invalid UTF-8 on three-byte runes")
	}
}

func TestIsAuthFailure(t *testing.T) {
	if isAuthFailure(errors.New("plain")) {
		t.Error("a plain error was reported as an auth failure")
	}
	auth := authFailure{errors.New("HTTP 401")}
	if !isAuthFailure(auth) {
		t.Error("an authFailure was not recognised")
	}
	if !isAuthFailure(fmt.Errorf("while sending: %w", auth)) {
		t.Error("a wrapped authFailure was not recognised")
	}
	if isAuthFailure(nil) {
		t.Error("nil was reported as an auth failure")
	}
}

func TestUserAgentsAreWellFormed(t *testing.T) {
	for _, ua := range []string{uaMAP, uaAlexa} {
		if strings.TrimSpace(ua) != ua {
			t.Errorf("%q has surrounding whitespace", ua)
		}
		if n := strings.Count(ua, "/"); n != 5 {
			t.Errorf("%q has %d slashes, want 5", ua, n)
		}
		if !strings.HasSuffix(ua, "/AEOBC") {
			t.Errorf("%q does not end in the device model", ua)
		}
	}
	if !strings.Contains(uaMAP, "MAPClientLib/130050002") {
		t.Error("uaMAP no longer names the MAP client library version")
	}
}

func TestOAuthErrorWithholdsAnUnrecognisedBody(t *testing.T) {
	body := []byte(`{"access_token":"Atna|NOTATOKENBUTPRETENDITIS"}`)
	got := oauthError(body, "")
	if strings.Contains(got, "NOTATOKENBUTPRETENDITIS") {
		t.Fatalf("the body reached the message: %q", got)
	}
}

func TestOAuthErrorKeepsTheNamedFields(t *testing.T) {
	body := []byte(`{"error":"invalid_grant","error_description":"the token is expired"}`)
	if want, got := "invalid_grant: the token is expired", oauthError(body, "Atnr|SOMETHINGELSE"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOAuthErrorStrikesTheTokenOutOfTheNamedFields(t *testing.T) {
	const token = "Atnr|PRETENDTHISISAREFRESHTOKEN"
	for _, body := range []string{
		`{"error":"invalid_grant","error_description":"bad ` + token + `"}`,
		`{"error":"` + token + `"}`,
	} {
		got := oauthError([]byte(body), token)
		if strings.Contains(got, token) {
			t.Errorf("the token reached the message: %q", got)
		}
		if !strings.Contains(got, "<token>") {
			t.Errorf("nothing marks where it was struck out: %q", got)
		}
	}
}

func TestOAuthErrorIsClipped(t *testing.T) {
	body := []byte(`{"error":"e","error_description":"` + strings.Repeat("x", 5000) + `"}`)
	if got := oauthError(body, ""); len(got) > 320 {
		t.Errorf("message is %d bytes, want it clipped", len(got))
	}
}

func TestEverySuCandidateIsAbsolute(t *testing.T) {
	if len(suPaths) == 0 {
		t.Fatal("no su candidates at all, so the exec falls back to $PATH every time")
	}
	for _, path := range suPaths {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("su candidate %q is relative", path)
		}
	}
}
