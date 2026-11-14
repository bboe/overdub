package alexa

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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

func TestTheUserAgentIsTheAlexaApps(t *testing.T) {
	if strings.TrimSpace(uaAlexa) != uaAlexa {
		t.Errorf("%q has surrounding whitespace", uaAlexa)
	}
	if n := strings.Count(uaAlexa, "/"); n != 5 {
		t.Errorf("%q has %d slashes, want 5", uaAlexa, n)
	}
	if !strings.HasPrefix(uaAlexa, "AmazonWebView/Amazon Alexa/") {
		t.Errorf("%q is not the Alexa app's shape; alexa.amazon.com is that app's host and"+
			" withholds the csrf cookie from an agent it does not know", uaAlexa)
	}
}

func TestTheCookieExchangeSendsWhatTheDotWould(t *testing.T) {
	var got *http.Request
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"response":{"tokens":{"cookies":{".amazon.com":[{"Name":"at-main","Value":"\"x\""}]}}}}`)
	}))
	defer srv.Close()
	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	u, _ := url.Parse(srv.URL)
	client.Transport = rewriteHost{host: u.Host, next: client.Transport}

	if err := exchangeCookies(client, "Atnr|token", alexaDomain); err != nil {
		t.Fatalf("exchangeCookies: %v", err)
	}
	if _, sent := got.Header["User-Agent"]; sent {
		t.Errorf("the exchange sent User-Agent %q; the Dot's own calls to api.amazon.com carry none", got.UserAgent())
	}
	want := url.Values{
		"app_name":             {appName},
		"source_token":         {"Atnr|token"},
		"source_token_type":    {"refresh_token"},
		"requested_token_type": {"auth_cookies"},
		"domain":               {".amazon.com"},
	}
	if form.Encode() != want.Encode() {
		t.Errorf("the form was %v, want %v: app_name is the one field Amazon insists on", form, want)
	}
	alexaURL, _ := url.Parse("https://alexa.amazon.com/")
	if cookies := client.Jar.Cookies(alexaURL); len(cookies) != 1 || cookies[0].Value != "x" {
		t.Errorf("the jar holds %v for alexa.amazon.com, want at-main=x with its quotes stripped", cookies)
	}
}

type rewriteHost struct {
	host string
	next http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = "http", r.host
	return r.next.RoundTrip(req)
}

func TestEveryAlexaRequestCarriesTheAppsHeaders(t *testing.T) {
	req, err := alexaRequest("GET", "https://alexa.amazon.com/api/language", nil)
	if err != nil {
		t.Fatal(err)
	}
	for header, want := range map[string]string{
		"User-Agent": uaAlexa, "Accept": "application/json", "Accept-Language": alexaLocale,
		"Accept-Charset": "utf-8", "Connection": "keep-alive",
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
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
