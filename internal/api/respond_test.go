package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/minhtt159/bank0/internal/config"
)

// A list endpoint must emit `[]` not `null` even when the DB returns a nil slice;
// non-slice values are untouched. (docs/09-fraudbank-integration.md §0.2)
func TestWriteJSONNilSliceIsEmptyArray(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil slice", []int(nil), "[]"},
		{"empty slice", []int{}, "[]"},
		{"populated slice", []int{1, 2}, "[1,2]"},
		{"object", map[string]int{"a": 1}, `{"a":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeJSON(rec, 200, c.in)
			if got := strings.TrimSpace(rec.Body.String()); got != c.want {
				t.Errorf("body = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOriginAllowed(t *testing.T) {
	allow := []string{"https://app.example", "http://localhost:5173"}
	if !originAllowed(allow, "http://localhost:5173") {
		t.Error("exact match should be allowed")
	}
	if originAllowed(allow, "https://evil.example") {
		t.Error("non-listed origin must be rejected")
	}
	if originAllowed(nil, "http://localhost:5173") {
		t.Error("empty allowlist must reject everything")
	}
}

func pgErr(code, msg string) error { return &pgconn.PgError{Code: code, Message: msg} }

func TestMapDBError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"unique", pgErr("23505", "dup"), 409, "already_exists"},
		{"insufficient", pgErr("23514", "insufficient available funds: have 1, need 2"), 422, "insufficient_funds"},
		{"idem", pgErr("23514", "idempotency key x reused with different parameters"), 422, "idempotency_key_conflict"},
		{"check_other", pgErr("23514", "amount must be positive"), 422, "unprocessable"},
		{"immutable", pgErr("23001", "ledger is append-only"), 409, "immutable"},
		{"in_progress", pgErr("55006", "request still in progress"), 409, "in_progress"},
		// The security-load-bearing branches: a foreign-debit / IDOR attempt (42501)
		// must be 403, and refresh-token reuse/expiry (28000/28P01) must be 401.
		{"forbidden_idor", pgErr("42501", "caller does not own debit account"), 403, "forbidden"},
		{"token_reuse", pgErr("28000", "refresh token reused"), 401, "unauthorized"},
		{"token_expired", pgErr("28P01", "refresh token expired"), 401, "unauthorized"},
		{"raise_notfound", pgErr("P0001", "account 123 not found"), 404, "not_found"},
		{"raise_state", pgErr("P0001", "cannot post transfer in state posted"), 409, "invalid_state"},
		{"raise_notactive", pgErr("P0001", "debit account not active"), 409, "invalid_state"},
		{"raise_other", pgErr("P0001", "this was rejected for reasons"), 422, "rejected"},
		{"invalid_input", pgErr("22P02", "invalid input syntax for type uuid: \"x\""), 400, "bad_request"},
		{"no_rows", pgx.ErrNoRows, 404, "not_found"},
		{"unknown", errors.New("boom"), 500, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			s.mapDBError(rec, req, c.err)
			if rec.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("bad json body: %v", err)
			}
			if body.Error != c.wantCode {
				t.Errorf("error code = %q, want %q", body.Error, c.wantCode)
			}
		})
	}
}

// The catch-all 500 branch MUST log the full underlying error server-side (so
// operators aren't blind to genuine internal faults) while the client body stays
// curated and leak-free. The mapped business branches must NOT log.
func TestMapDBErrorLogsUnmapped500(t *testing.T) {
	var logBuf bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))}

	// Unmapped error -> 500: the raw error must reach the log, not the client.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.mapDBError(rec, req, errors.New("function does_not_exist() does not exist"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "does_not_exist") {
		t.Errorf("client body leaked the raw error: %q", rec.Body.String())
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "unmapped db error -> 500") {
		t.Errorf("500 path did not log the error; log = %q", logged)
	}
	if !strings.Contains(logged, "does_not_exist") {
		t.Errorf("500 log did not carry the underlying error; log = %q", logged)
	}

	// A mapped business branch (here 23505 -> 409) must not log.
	logBuf.Reset()
	rec = httptest.NewRecorder()
	s.mapDBError(rec, req, pgErr("23505", "dup"))
	if logBuf.Len() != 0 {
		t.Errorf("mapped business branch should not log; got %q", logBuf.String())
	}
}

// An internal-invariant RAISE (ERRCODE XX000, internal_error) must hit the curated
// 500 catch-all: the client body stays leak-free while the full raise text reaches
// the log. Mirrors TestMapDBErrorLogsUnmapped500 for the SQLSTATE-carrying path.
func TestMapDBErrorInternalErrcode500(t *testing.T) {
	var logBuf bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.mapDBError(rec, req, pgErr("XX000", "EXTERNAL_CLEARING system account missing (run seed)"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "EXTERNAL_CLEARING") {
		t.Errorf("client body leaked the raise text: %q", rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "internal" {
		t.Errorf("body error = %q (err %v), want %q", body.Error, err, "internal")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "unmapped db error -> 500") || !strings.Contains(logged, "EXTERNAL_CLEARING") {
		t.Errorf("XX000 must be logged with the underlying error; log = %q", logged)
	}
	if !strings.Contains(logged, "XX000") {
		t.Errorf("500 log should carry the SQLSTATE; log = %q", logged)
	}
}

// A malformed path param is rejected by the generated binding layer BEFORE the
// handler runs. With the curated ErrorHandlerFunc wired in Router(), that rejection
// must be the standard JSON error shape (application/json, {"error":"bad_request"}),
// not oapi-codegen's default text/plain http.Error. No DB is touched: requireJWT
// only validates the token and the binding fails ahead of any handler, so this runs
// without a DSN.
func TestBindingErrorReturnsJSON(t *testing.T) {
	cfg := config.Config{
		App:    config.AppConfig{Name: "bank0", Version: "test", Env: "development"},
		Server: config.ServerConfig{Mode: "api", DefaultPageLimit: 25},
		Auth: config.AuthConfig{
			JWTSecret: "test-secret", JWTTTL: time.Hour, JWTIssuer: "bank0", JWTAudience: "bank0-client",
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(cfg, log, nil)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	tok, _, err := s.issueJWT(uuid.New(), "customer", "alice", []string{"pwd"}, "")
	if err != nil {
		t.Fatalf("issueJWT: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/transfers/not-a-uuid", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode json body: %v", err)
	}
	if body.Error != "bad_request" {
		t.Errorf("error code = %q, want bad_request", body.Error)
	}
}

func TestDBErrorMessage(t *testing.T) {
	if got := dbErrorMessage(pgx.ErrNoRows); got != "not found" {
		t.Errorf("ErrNoRows message = %q", got)
	}
	// A crafted business SQLSTATE (P0001, 23514, ...) surfaces its developer-authored
	// message verbatim to the console flash.
	if got := dbErrorMessage(pgErr("23514", "insufficient funds")); got != "insufficient funds" {
		t.Errorf("crafted PgError should surface its Message; got %q", got)
	}
	if got := dbErrorMessage(pgErr("P0001", "account not found")); got != "account not found" {
		t.Errorf("P0001 should be echoed; got %q", got)
	}
	// A native, non-whitelisted PgError (here 23503 foreign_key_violation, whose raw
	// message names the constraint) is curated to a generic string — no schema leak.
	if got := dbErrorMessage(pgErr("23503", `insert or update on table "x" violates foreign key constraint "x_y_fkey"`)); got != "database error" {
		t.Errorf("non-whitelisted PgError should be generic; got %q", got)
	}
	if got := dbErrorMessage(errors.New("boom")); got != "internal error" {
		t.Errorf("generic error message = %q", got)
	}
}
