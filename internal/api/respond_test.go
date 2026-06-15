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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func TestDBErrorMessage(t *testing.T) {
	if got := dbErrorMessage(pgx.ErrNoRows); got != "not found" {
		t.Errorf("ErrNoRows message = %q", got)
	}
	if got := dbErrorMessage(pgErr("23514", "insufficient funds")); got != "insufficient funds" {
		t.Errorf("PgError should surface its Message; got %q", got)
	}
	if got := dbErrorMessage(errors.New("boom")); got != "internal error" {
		t.Errorf("generic error message = %q", got)
	}
}
