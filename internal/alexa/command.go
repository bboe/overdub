package alexa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bboe/overdub/internal/device"
)

const (
	mapUID  = "32051"
	mapDir  = "/data/local/map"
	mapJar  = mapDir + "/mapdump.jar"
	mapKey  = "com.amazon.dcp.sso.token.oauth.amazon.refresh_token"
	mapMain = "MapDump"

	uaMAP = "AmazonWebView/MAPClientLib/130050002/Android/5.1.1/AEOBC"

	uaAlexa = "AmazonWebView/AmazonAlexa/2.2.223830.0/Android/5.1.1/AEOBC"

	exchangePath = "/ap/exchangetoken/cookies"

	alexaDomain = "amazon.com"
	alexaLocale = "en-US"

	maxBody = 1 << 20
)

type Client struct {
	mu sync.Mutex

	token string

	http *http.Client
}

const JarPath = mapJar

func JarInstalled() bool {
	_, err := os.Stat(mapJar)
	return err == nil
}

func NewClient() *Client { return &Client{} }

func (a *Client) Send(text string) error {
	if !a.mu.TryLock() {
		return fmt.Errorf("another command is still in flight")
	}
	defer a.mu.Unlock()

	err := a.attempt(text)
	if err == nil {
		return nil
	}
	if !isAuthFailure(err) {
		return err
	}
	a.token = ""
	if again := a.attempt(text); again != nil {
		return fmt.Errorf("refused, and again after re-extracting the token: %w", again)
	}
	return nil
}

func (a *Client) attempt(text string) error {
	if a.token == "" {
		token, err := extractToken()
		if err != nil {
			return fmt.Errorf("extract token: %w", err)
		}
		a.token = token
	}

	client := a.httpClient()

	if err := exchangeCookies(client, a.token, alexaDomain); err != nil {
		return err
	}
	base := "https://alexa." + alexaDomain

	devices, err := listDevices(client, base)
	if err != nil {
		return err
	}
	target, err := pick(devices)
	if err != nil {
		return err
	}
	csrf, err := csrfToken(client, base)
	if err != nil {
		return err
	}
	return sendText(client, base, csrf, target, text, alexaLocale)
}

func pick(devices []Device) (Device, error) {
	serial := strings.TrimSpace(device.Getprop("ro.serialno"))
	for _, d := range devices {
		if serial != "" && strings.EqualFold(d.SerialNumber, serial) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("no device on the account matches this Dot's serial %q", serial)
}

var suPaths = []string{"/sbin/su", "/system/xbin/su", "/system/bin/su"}

func suPath() string {
	for _, path := range suPaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "su"
}

func extractToken() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, suPath(), mapUID, "-c",
		fmt.Sprintf("CLASSPATH=%s app_process %s %s %s", mapJar, mapDir, mapMain, mapKey))
	var token, diag bytes.Buffer
	cmd.Stdout = &token
	cmd.Stderr = &diag
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, clip(diag.Bytes()))
	}
	value := strings.TrimSpace(token.String())
	if value == "" {
		return "", fmt.Errorf("MapDump exited 0 with nothing on stdout: %s", clip(diag.Bytes()))
	}
	if !tokenShaped(value) {
		return "", fmt.Errorf("MapDump wrote %d bytes that are not a token: %s",
			len(value), clip(diag.Bytes()))
	}
	return value, nil
}

const (
	minToken = 16
	maxToken = 4096
)

func tokenShaped(value string) bool {
	if len(value) < minToken || len(value) > maxToken {
		return false
	}
	return strings.IndexFunc(value, unicode.IsSpace) < 0
}

type authFailure struct{ error }

func isAuthFailure(err error) bool {
	var a authFailure
	return errors.As(err, &a)
}

func (a *Client) httpClient() *http.Client {
	if a.http == nil {
		a.http = newAlexaHTTPClient()
	}
	jar, _ := cookiejar.New(nil)
	a.http.Jar = jar
	return a.http
}

func newAlexaHTTPClient() *http.Client {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var servers []string
			for _, prop := range []string{"net.dns1", "net.dns2"} {
				if s := strings.TrimSpace(device.Getprop(prop)); s != "" {
					servers = append(servers, net.JoinHostPort(s, "53"))
				}
			}
			if len(servers) == 0 {
				return nil, fmt.Errorf("no net.dns1/net.dns2 to resolve with")
			}
			var d net.Dialer
			var err error
			for _, s := range servers {
				var conn net.Conn
				if conn, err = d.DialContext(ctx, network, s); err == nil {
					return conn, nil
				}
			}
			return nil, err
		},
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, Resolver: resolver}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:     dialer.DialContext,
			IdleConnTimeout: 90 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > 0 && via[0].URL.Path == exchangePath {
				return fmt.Errorf("refusing to follow a redirect away from %s", exchangePath)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

func oauthError(body []byte, token string) string {
	withhold := func(s string) string {
		if token != "" {
			s = strings.ReplaceAll(s, token, "<token>")
		}
		return clip([]byte(s))
	}
	var out struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Error == "" {
		return "(no error field; the body is withheld, it can carry the token)"
	}
	if out.Description == "" {
		return withhold(out.Error)
	}
	return withhold(out.Error + ": " + out.Description)
}

func exchangeCookies(client *http.Client, token, domain string) error {
	form := url.Values{
		"di.os.name":           {"FireOS"},
		"app_name":             {"Amazon Alexa"},
		"source_token":         {token},
		"source_token_type":    {"refresh_token"},
		"requested_token_type": {"auth_cookies"},
		"domain":               {"." + domain},
	}
	req, err := http.NewRequest("POST", "https://api.amazon.com"+exchangePath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-amzn-identity-auth-domain", "api.amazon.com")
	req.Header.Set("User-Agent", uaMAP)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return authFailure{fmt.Errorf("cookie exchange: HTTP %d: %s", resp.StatusCode, oauthError(body, token))}
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("cookie exchange: HTTP %d: %s", resp.StatusCode, oauthError(body, token))
	}

	var out struct {
		Response struct {
			Tokens struct {
				Cookies map[string][]struct {
					Name  string `json:"Name"`
					Value string `json:"Value"`
				} `json:"cookies"`
			} `json:"tokens"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("cookie exchange: parse: %w", err)
	}
	var cookies []*http.Cookie
	for _, list := range out.Response.Tokens.Cookies {
		for _, ck := range list {
			cookies = append(cookies, &http.Cookie{
				Name:   ck.Name,
				Value:  strings.Trim(ck.Value, `"`),
				Domain: "." + domain,
				Path:   "/",
			})
		}
	}
	if len(cookies) == 0 {
		return authFailure{errors.New("cookie exchange returned no cookies")}
	}
	for _, host := range []string{"https://alexa." + domain, "https://" + domain} {
		u, _ := url.Parse(host)
		client.Jar.SetCookies(u, cookies)
	}
	return nil
}

func alexaRequest(method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", uaAlexa)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("DNT", "1")
	req.Header.Set("Connection", "keep-alive")
	return req, nil
}

func csrfToken(client *http.Client, base string) (string, error) {
	u, _ := url.Parse(base)
	inJar := func() string {
		for _, ck := range client.Jar.Cookies(u) {
			if ck.Name == "csrf" {
				return ck.Value
			}
		}
		return ""
	}
	if v := inJar(); v != "" {
		return v, nil
	}
	req, err := alexaRequest("GET", base+"/api/language", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Referer", base+"/spa/index.html")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()

	if v := inJar(); v != "" {
		return v, nil
	}
	return "", authFailure{errors.New("no csrf cookie after an authenticated GET")}
}

type Device struct {
	DeviceType   string `json:"deviceType"`
	SerialNumber string `json:"serialNumber"`
	CustomerID   string `json:"deviceOwnerCustomerId"`
}

func listDevices(client *http.Client, base string) ([]Device, error) {
	req, err := alexaRequest("GET", base+"/api/devices-v2/device", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", base+"/spa/index.html")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, authFailure{fmt.Errorf("device list: HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("device list: HTTP %d: %s", resp.StatusCode, clip(body))
	}
	var out struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, authFailure{fmt.Errorf("device list: parse: %w", err)}
	}
	if len(out.Devices) == 0 {
		return nil, authFailure{errors.New("device list: the account lists no devices at all")}
	}
	return out.Devices, nil
}

func sendText(client *http.Client, base, csrf string, target Device, text, locale string) error {
	sequence, _ := json.Marshal(map[string]any{
		"@type": "com.amazon.alexa.behaviors.model.Sequence",
		"startNode": map[string]any{
			"@type": "com.amazon.alexa.behaviors.model.OpaquePayloadOperationNode",
			"type":  "Alexa.TextCommand",
			"operationPayload": map[string]string{
				"deviceType":         target.DeviceType,
				"deviceSerialNumber": target.SerialNumber,
				"locale":             locale,
				"customerId":         target.CustomerID,
				"text":               text,
			},
		},
	})
	body, _ := json.Marshal(map[string]string{
		"behaviorId":   "PREVIEW",
		"sequenceJson": string(sequence),
		"status":       "ENABLED",
	})

	req, err := alexaRequest("POST", base+"/api/behaviors/preview", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("csrf", csrf)
	req.Header.Set("Referer", base+"/spa/index.html")
	req.Header.Set("Origin", base)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return authFailure{fmt.Errorf("behaviors/preview: HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("behaviors/preview: HTTP %d: %s", resp.StatusCode, clip(rb))
	}
	return nil
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		i := 300
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i] + "..."
	}
	return s
}
