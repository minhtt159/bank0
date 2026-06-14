package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// writeJSON encodes v as the response body. A nil top-level slice is coerced to an
// empty slice so list endpoints always emit `[]`, never `null`: a JSON array
// endpoint must stay an array (typed clients like [LedgerEntry] / List<Transfer>
// decode-fail on null). This is the single, audit-proof guarantee for every list
// handler — present and future (disputes, list-my-transfers, ...). See
// docs/09-fraudbank-bff-plan.md §0.2.
func writeJSON(w http.ResponseWriter, status int, v any) {
	if v != nil {
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
			v = reflect.MakeSlice(rv.Type(), 0, 0).Interface()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: code, Message: msg})
}

// mapDBError is the ONLY place the API encodes business knowledge: it translates
// the typed errors the PL/pgSQL functions raise into HTTP status codes. Every
// rule itself still lives in the database (see docs/03-...md §5).
func mapDBError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		msg := pg.Message
		switch pg.Code {
		case "23505": // unique_violation
			writeError(w, http.StatusConflict, "already_exists", msg)
			return
		case "23514": // check_violation (insufficient funds, key reuse, invalid state, ...)
			code := "unprocessable"
			if strings.Contains(msg, "insufficient") {
				code = "insufficient_funds"
			} else if strings.Contains(msg, "idempotency key") {
				code = "idempotency_key_conflict"
			}
			writeError(w, http.StatusUnprocessableEntity, code, msg)
			return
		case "23001": // restrict_violation (append-only ledger / balance tamper guard)
			writeError(w, http.StatusConflict, "immutable", msg)
			return
		case "55006": // object_in_use (idempotent request still in progress)
			writeError(w, http.StatusConflict, "in_progress", msg)
			return
		case "28000", "28P01": // refresh-token reuse/expiry/unknown -> re-authenticate
			writeError(w, http.StatusUnauthorized, "unauthorized", msg)
			return
		case "42501": // insufficient_privilege: caller doesn't own the debit account
			writeError(w, http.StatusForbidden, "forbidden", msg)
			return
		case "P0001": // generic RAISE EXCEPTION — disambiguate by message
			switch {
			case strings.Contains(msg, "not found"), strings.Contains(msg, "does not exist"):
				writeError(w, http.StatusNotFound, "not_found", msg)
			case strings.Contains(msg, "not active"), strings.Contains(msg, "cannot "):
				writeError(w, http.StatusConflict, "invalid_state", msg)
			default:
				writeError(w, http.StatusUnprocessableEntity, "rejected", msg)
			}
			return
		}
	}

	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// dbErrorMessage extracts a human-readable message from a DB error, for console
// flash messages (the JSON API uses mapDBError instead).
func dbErrorMessage(err error) string {
	if errors.Is(err, pgx.ErrNoRows) {
		return "not found"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Message
	}
	return "internal error"
}

// maxJSONBody caps request bodies so a giant payload can't exhaust memory.
const maxJSONBody = 1 << 20 // 1 MiB

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

// decodeOptionalJSON decodes if a body is present; it never writes a response,
// so it's safe for endpoints where the body (e.g. an optional reason) may be empty.
func decodeOptionalJSON(r *http.Request, dst any) {
	_ = json.NewDecoder(r.Body).Decode(dst)
}

