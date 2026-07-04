package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/db"
)

// capNotifier captures dispatched verification codes (the out-of-band channel)
// so the tests can complete the loop without email/SMS.
type capNotifier struct {
	mu    sync.Mutex
	codes []string
	dests []string
}

func (c *capNotifier) SendVerification(_ context.Context, _, destination, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codes = append(c.codes, code)
	c.dests = append(c.dests, destination)
	return nil
}

func (c *capNotifier) last(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.codes) == 0 {
		t.Fatal("no verification code was dispatched")
	}
	return c.codes[len(c.codes)-1]
}

// newRegTestServer is newTestServer plus a captured notifier (the *Server handle
// is needed to swap the field, so it can't reuse the shared helper directly).
func newRegTestServer(t *testing.T) (*httptest.Server, *db.Postgres, *capNotifier) {
	t.Helper()
	if testDSN == "" {
		t.Skip("set TEST_DATABASE_DSN to run HTTP integration tests")
	}
	pg, err := db.NewPostgres(config.DatabaseConfig{
		DSN: testDSN, MaxOpenConns: 5, MaxIdleConns: 2, ConnTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cfg := config.Config{
		App:    config.AppConfig{Name: "bank0", Version: "test", Env: "development"},
		Server: config.ServerConfig{Mode: "api", DefaultPageLimit: 25},
		Auth:   config.AuthConfig{JWTSecret: "test-secret", JWTTTL: time.Hour, JWTIssuer: "bank0", JWTAudience: "bank0-client"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, log, pg)
	cap := &capNotifier{}
	srv.notifier = cap
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(func() { ts.Close(); pg.Close() })
	return ts, pg, cap
}

func postJSON(t *testing.T, url string, hdr map[string]string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func TestHTTPRegisterVerifyLoginFlow(t *testing.T) {
	ts, _, cap := newRegTestServer(t)
	uname := "flow" + uhex(10)
	email := uname + "@example.com"
	key := uuid.NewString()

	// Register (no bearer needed): 201, body carries the verify_token, never the code.
	resp := postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": key},
		map[string]string{"username": uname, "password": "correct-horse-battery", "full_name": "Flow T", "email": email})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d, want 201: %s", resp.StatusCode, body(t, resp))
	}
	var reg struct {
		UserID           string `json:"user_id"`
		OnboardingStatus string `json:"onboarding_status"`
		VerifyChannel    string `json:"verify_channel"`
		VerifyToken      string `json:"verify_token"`
	}
	decodeBody(t, resp, &reg)
	if reg.VerifyToken == "" || reg.OnboardingStatus != "pending_verification" || reg.VerifyChannel != "email" {
		t.Fatalf("register body = %+v", reg)
	}
	code := cap.last(t)
	if len(code) != 6 {
		t.Fatalf("captured code %q, want 6 digits", code)
	}

	// Login before verification is denied.
	r := postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "correct-horse-battery"})
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("login before verify = %d, want 401", r.StatusCode)
	}

	// Replay the SAME registration: 201 again, same user_id, replay header, no dup dispatch.
	sent := len(cap.codes)
	r = postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": key},
		map[string]string{"username": uname, "password": "correct-horse-battery", "full_name": "Flow T", "email": email})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("replayed register = %d, want 201", r.StatusCode)
	}
	if r.Header.Get("Idempotency-Replayed") != "true" {
		t.Error("replay must set Idempotency-Replayed: true")
	}
	var reg2 struct {
		UserID      string `json:"user_id"`
		VerifyToken string `json:"verify_token"`
	}
	decodeBody(t, r, &reg2)
	if reg2.UserID != reg.UserID || reg2.VerifyToken != reg.VerifyToken {
		t.Errorf("replay body differs: %+v vs %+v", reg2, reg)
	}
	if len(cap.codes) != sent {
		t.Error("replay must not re-dispatch a code")
	}

	// Wrong code -> 401.
	wrong := "000000"
	if wrong == code {
		wrong = "000001"
	}
	r = postJSON(t, ts.URL+"/auth/verify-contact", nil, map[string]string{"verify_token": reg.VerifyToken, "code": wrong})
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong code = %d, want 401", r.StatusCode)
	}

	// Right code -> 200 login_ready.
	r = postJSON(t, ts.URL+"/auth/verify-contact", nil, map[string]string{"verify_token": reg.VerifyToken, "code": code})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("verify = %d, want 200: %s", r.StatusCode, body(t, r))
	}
	var ver struct {
		LoginReady       bool   `json:"login_ready"`
		OnboardingStatus string `json:"onboarding_status"`
	}
	decodeBody(t, r, &ver)
	if !ver.LoginReady || ver.OnboardingStatus != "verified" {
		t.Fatalf("verify body = %+v", ver)
	}

	// Login now works and /me shows the onboarding state.
	r = postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "correct-horse-battery"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("login after verify = %d, want 200", r.StatusCode)
	}
	var login struct {
		Token string `json:"token"`
	}
	decodeBody(t, r, &login)
	if login.Token == "" {
		t.Fatal("no access token")
	}
	me := get(t, http.DefaultClient, ts.URL+"/me", map[string]string{"Authorization": "Bearer " + login.Token})
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/me = %d, want 200", me.StatusCode)
	}
	if b := body(t, me); !strings.Contains(b, `"onboarding_status":"verified"`) {
		t.Errorf("/me should expose onboarding_status=verified: %s", b)
	}
}

func TestHTTPRegisterValidationAndThrottles(t *testing.T) {
	ts, _, _ := newRegTestServer(t)

	// Missing Idempotency-Key -> 400.
	r := postJSON(t, ts.URL+"/auth/register", nil,
		map[string]string{"username": "x" + uhex(8), "password": "correct-horse-battery", "full_name": "X", "email": "x@example.com"})
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("no key = %d, want 400", r.StatusCode)
	}

	// Weak password -> 422.
	r = postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": "x" + uhex(8), "password": "short", "full_name": "X", "email": "x" + uhex(6) + "@example.com"})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("weak password = %d, want 422", r.StatusCode)
	}

	// No contact channel -> 422.
	r = postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": "x" + uhex(8), "password": "correct-horse-battery", "full_name": "X"})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no channel = %d, want 422", r.StatusCode)
	}

	// Duplicate username -> 409.
	uname := "dupu" + uhex(8)
	r = postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": uname, "password": "correct-horse-battery", "full_name": "X", "email": "a" + uhex(6) + "@example.com"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("first register = %d, want 201", r.StatusCode)
	}
	r = postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": uname, "password": "correct-horse-battery", "full_name": "X", "email": "b" + uhex(6) + "@example.com"})
	if r.StatusCode != http.StatusConflict {
		t.Errorf("duplicate register = %d, want 409", r.StatusCode)
	}
}

func TestHTTPResendCode(t *testing.T) {
	ts, _, cap := newRegTestServer(t)
	uname := "rs" + uhex(10)
	r := postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": uname, "password": "correct-horse-battery", "full_name": "X", "email": uname + "@example.com"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d, want 201", r.StatusCode)
	}
	var reg struct {
		VerifyToken string `json:"verify_token"`
	}
	decodeBody(t, r, &reg)

	// Within the 60s cooldown -> 429.
	r = postJSON(t, ts.URL+"/auth/resend-code", nil, map[string]string{"verify_token": reg.VerifyToken})
	if r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("resend within cooldown = %d, want 429", r.StatusCode)
	}

	// Unknown token -> silent 202 (no probe oracle), nothing dispatched.
	sent := len(cap.codes)
	r = postJSON(t, ts.URL+"/auth/resend-code", nil, map[string]string{"verify_token": "no-such-token"})
	if r.StatusCode != http.StatusAccepted {
		t.Errorf("unknown-token resend = %d, want 202", r.StatusCode)
	}
	if len(cap.codes) != sent {
		t.Error("unknown-token resend must not dispatch")
	}
}
