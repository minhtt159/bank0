package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/migrate"
)

// HTTP integration tests drive the real router (auth middleware, routing, RBAC,
// ownership, the HTML/JSON contract) against a real Postgres. They skip unless
// TEST_DATABASE_DSN is set (see `task test:db`).

var testDSN = os.Getenv("TEST_DATABASE_DSN")

func TestMain(m *testing.M) {
	if testDSN != "" {
		if err := migrate.Up(testDSN); err != nil {
			panic("migrate test db: " + err.Error())
		}
	}
	os.Exit(m.Run())
}

func newTestServer(t *testing.T) (*httptest.Server, *db.Postgres) {
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
		App:    config.AppConfig{Name: "bank0", Version: "test", Env: "development"}, // dev => cookie not Secure-only
		Server: config.ServerConfig{Mode: "all", DefaultPageLimit: 25},
		Admin:  config.AdminConfig{MakerCheckerThresholdMinor: 1_000_000, SessionIdleTimeout: 30 * time.Minute},
		Auth:   config.AuthConfig{JWTSecret: "test-secret", JWTTTL: time.Hour, JWTIssuer: "bank0", JWTAudience: "bank0-client"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(NewServer(cfg, log, pg).Router())
	t.Cleanup(func() { ts.Close(); pg.Close() })
	return ts, pg
}

// --- fixtures ------------------------------------------------------------

func uhex(n int) string { return strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:n] }

func mkUser(t *testing.T, pg *db.Postgres, role sqlc.UserRole) (uuid.UUID, string) {
	t.Helper()
	name := "u" + uhex(16)
	id, err := pg.Queries.CreateUser(context.Background(), sqlc.CreateUserParams{
		Username: name, Password: "pw", FullName: "T", Role: role,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id, name
}

func mkAcct(t *testing.T, pg *db.Postgres, owner uuid.UUID, fundMinor int64) uuid.UUID {
	t.Helper()
	id, err := pg.Queries.CreateAccount(context.Background(), sqlc.CreateAccountParams{
		UserID: owner, Iban: "SE" + uhex(22), Pin: "1234", TransferLimitMinor: 100_000_000,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if fundMinor > 0 {
		if _, err := pg.Queries.Deposit(context.Background(), sqlc.DepositParams{
			IdempotencyKey: uuid.NewString(), AccountID: id, AmountMinor: fundMinor, Description: "fund",
		}); err != nil {
			t.Fatalf("fund: %v", err)
		}
	}
	return id
}

// noRedirect client so we can assert on 303s.
func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, c *http.Client, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// --- portal auth + RBAC --------------------------------------------------

func TestHTTPPortalAuthAndRBAC(t *testing.T) {
	ts, pg := newTestServer(t)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, audName := mkUser(t, pg, sqlc.UserRoleAuditor)
	acct := mkAcct(t, pg, func() uuid.UUID { id, _ := mkUser(t, pg, sqlc.UserRoleCustomer); return id }(), 5_000)

	anon := newClient()

	// public probe
	if r := get(t, anon, ts.URL+"/health", nil); r.StatusCode != 200 {
		t.Errorf("/health = %d, want 200", r.StatusCode)
	}
	// unauthenticated JSON caller -> 401
	if r := get(t, anon, ts.URL+"/admin/reconcile", nil); r.StatusCode != 401 {
		t.Errorf("unauth /admin/reconcile = %d, want 401", r.StatusCode)
	}
	// unauthenticated browser -> 303 redirect to /login
	if r := get(t, anon, ts.URL+"/", map[string]string{"Accept": "text/html"}); r.StatusCode != 303 {
		t.Errorf("unauth GET / (html) = %d, want 303", r.StatusCode)
	}

	admin := login(t, ts, adminName, "pw")
	// panel is the chrome (search + lazy results container)...
	if r := get(t, admin, ts.URL+"/console/users", nil); r.StatusCode != 200 {
		t.Errorf("admin /console/users = %d, want 200", r.StatusCode)
	}
	// ...the rows (and usernames) come from the results fragment
	if r := get(t, admin, ts.URL+"/console/users/results", nil); r.StatusCode != 200 || !strings.Contains(body(t, r), adminName) {
		t.Errorf("admin /console/users/results not 200/listing %s", adminName)
	}
	if r := get(t, admin, ts.URL+"/admin/reconcile", nil); r.StatusCode != 200 {
		t.Errorf("admin /admin/reconcile = %d, want 200", r.StatusCode)
	}

	aud := login(t, ts, audName, "pw")
	if r := get(t, aud, ts.URL+"/console/users/new", nil); r.StatusCode != 403 {
		t.Errorf("auditor new-user form = %d, want 403", r.StatusCode)
	}
	// auditor cannot move money
	r, _ := aud.PostForm(ts.URL+"/console/accounts/"+acct.String()+"/credit",
		url.Values{"amount": {"10"}, "idempotency_key": {uuid.NewString()}})
	if r.StatusCode != 403 {
		t.Errorf("auditor credit = %d, want 403", r.StatusCode)
	}
}

func login(t *testing.T, ts *httptest.Server, username, password string) *http.Client {
	t.Helper()
	c := newClient()
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"username": {username}, "password": {password}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	return c
}

// --- client JWT + ownership (IDOR) ---------------------------------------

func TestHTTPClientJWTOwnership(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 10_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 10_000)

	anon := newClient()
	// no bearer -> 401
	if r := get(t, anon, ts.URL+"/accounts/"+aliceAcct.String(), nil); r.StatusCode != 401 {
		t.Errorf("no-token account = %d, want 401", r.StatusCode)
	}

	tok := clientToken(t, ts, aliceName, "pw")
	bearer := map[string]string{"Authorization": "Bearer " + tok}

	if r := get(t, anon, ts.URL+"/accounts/"+aliceAcct.String(), bearer); r.StatusCode != 200 {
		t.Errorf("own account = %d, want 200", r.StatusCode)
	}
	// IDOR: alice must not see bob's account
	if r := get(t, anon, ts.URL+"/accounts/"+bobAcct.String(), bearer); r.StatusCode != 404 {
		t.Errorf("other's account = %d, want 404 (IDOR blocked)", r.StatusCode)
	}

	// transfer: debit must be owned. Debiting bob's account -> 403.
	mkTransfer := func(debit, credit uuid.UUID) int {
		b := `{"debit_account":"` + debit.String() + `","credit_account":"` + credit.String() + `","amount_minor":100}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/transfers", strings.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set("Content-Type", "application/json")
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatalf("transfer: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := mkTransfer(bobAcct, aliceAcct); code != 403 {
		t.Errorf("debit not-owned = %d, want 403", code)
	}
	if code := mkTransfer(aliceAcct, bobAcct); code != 200 {
		t.Errorf("debit owned = %d, want 200", code)
	}
}

func clientToken(t *testing.T, ts *httptest.Server, username, password string) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/auth/login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatalf("client login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client login status = %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("no token in login response: %v", err)
	}
	return out.Token
}
