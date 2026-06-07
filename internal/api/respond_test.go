package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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
		{"raise_notfound", pgErr("P0001", "account 123 not found"), 404, "not_found"},
		{"raise_state", pgErr("P0001", "cannot post transfer in state posted"), 409, "invalid_state"},
		{"raise_notactive", pgErr("P0001", "debit account not active"), 409, "invalid_state"},
		{"raise_other", pgErr("P0001", "this was rejected for reasons"), 422, "rejected"},
		{"no_rows", pgx.ErrNoRows, 404, "not_found"},
		{"unknown", errors.New("boom"), 500, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapDBError(rec, c.err)
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
