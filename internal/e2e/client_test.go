//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// Thin local HTTP helpers. Deliberately self-contained — the suite is black-box and
// does NOT import the internal/api test fixtures.

// noRedirect keeps 3xx responses intact so a test can assert on the redirect itself
// (the portal login replies 303 -> /).
func noRedirectJar() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func plainClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// portalSession is an operator session: a cookie jar carrying the bank0_session
// cookie obtained from POST /login on the portal binary.
type portalSession struct {
	t      *testing.T
	base   string
	client *http.Client
}

// loginPortal performs the operator console login (form POST -> 303 + Set-Cookie).
func loginPortal(t *testing.T, base, username, password string) *portalSession {
	t.Helper()
	c := noRedirectJar()
	resp, err := c.PostForm(base+"/login", url.Values{
		"username": {username},
		"password": {password},
	})
	if err != nil {
		t.Fatalf("portal login POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("portal login status = %d, want 303 (body=%.200s)", resp.StatusCode, readBody(t, resp))
	}
	u, _ := url.Parse(base)
	if len(c.Jar.Cookies(u)) == 0 {
		t.Fatalf("portal login set no cookies")
	}
	return &portalSession{t: t, base: base, client: c}
}

func (p *portalSession) get(path string) *http.Response {
	p.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, p.base+path, nil)
	resp, err := p.client.Do(req)
	if err != nil {
		p.t.Fatalf("portal GET %s: %v", path, err)
	}
	return resp
}

// postJSON sends a JSON body with the session cookie. No Origin/Referer header is
// set, so it passes the portal's same-origin CSRF guard (a non-browser caller).
func (p *portalSession) postJSON(path string, body any) *http.Response {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, p.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		p.t.Fatalf("portal POST %s: %v", path, err)
	}
	return resp
}

// postJSONIdem is postJSON plus an Idempotency-Key header (money moves carry one).
func (p *portalSession) postJSONIdem(path, idemKey string, body any) *http.Response {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, p.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idemKey)
	resp, err := p.client.Do(req)
	if err != nil {
		p.t.Fatalf("portal POST %s: %v", path, err)
	}
	return resp
}

// apiSession is a client (customer) session: a JWT bearer token from POST /auth/login
// on the api binary, plus the refresh token.
type apiSession struct {
	t      *testing.T
	base   string
	client *http.Client
	token  string
	userID string
}

func loginAPI(t *testing.T, base, username, password string) *apiSession {
	t.Helper()
	c := plainClient()
	resp, err := c.Post(base+"/auth/login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatalf("api login POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api login status = %d, want 200 (body=%.200s)", resp.StatusCode, readBody(t, resp))
	}
	var out struct {
		Token  string `json:"token"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("api login: no token in response: %v", err)
	}
	return &apiSession{t: t, base: base, client: c, token: out.Token, userID: out.UserID}
}

func (a *apiSession) get(path string) *http.Response {
	a.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, a.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatalf("api GET %s: %v", path, err)
	}
	return resp
}

func (a *apiSession) postJSONIdem(path, idemKey string, body any) *http.Response {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, a.base+path, r)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatalf("api POST %s: %v", path, err)
	}
	return resp
}

// --- raw request helpers (used to probe routes without a real auth context) ----

// rawGet issues a bare GET with optional headers and returns the status + body. Used
// to assert that a route is ABSENT (404) on the wrong surface, or that auth is
// rejected (401/403).
func rawGet(t *testing.T, base, path string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := plainClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", base, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(t, resp)
}

func rawPost(t *testing.T, base, path string, hdr map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := plainClient().Do(req)
	if err != nil {
		t.Fatalf("POST %s%s: %v", base, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(t, resp)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func decodeJSONBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode JSON body: %v", err)
	}
}
