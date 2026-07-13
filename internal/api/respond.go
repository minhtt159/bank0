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
// docs/09-fraudbank-integration.md §0.2.
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

// bindingErrorJSON is the oapi-codegen ErrorHandlerFunc for BOTH generated
// packages (genclient/genadmin). The generated default emits text/plain via
// http.Error; this replaces it so a malformed path/query/header param yields the
// standard JSON error shape, matching every other error the API returns.
func bindingErrorJSON(w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, http.StatusBadRequest, "bad_request", err.Error())
}

// mapDBError is the ONLY place the API encodes business knowledge: it translates
// the typed errors the PL/pgSQL functions raise into HTTP status codes. Every
// rule itself still lives in the database (see docs/03-...md §5).
//
// It is a *Server method so it can reach the per-request logger (s.logFor) and log
// the FULL underlying error on the catch-all 500 branch — operators were otherwise
// blind to genuine internal faults (e.g. a missing function), since the client only
// ever sees the curated, leak-free body. The mapped business branches (404/409/422/
// 403/401) are expected outcomes and are intentionally NOT logged here.
func (s *Server) mapDBError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		// msg is the raw Postgres exception text. We echo it ONLY for developer-
		// authored business RAISEs (P0001, and the crafted check_violation messages
		// below) which are meaningful + safe for the client. For raw, Postgres-
		// generated messages (a unique-constraint name, a generic check trip) we
		// return a stable curated message instead, so internal schema details don't
		// leak to client-scoped/unauthenticated callers.
		msg := pg.Message
		switch pg.Code {
		case "23505": // unique_violation (raw message names the constraint — curate)
			writeError(w, http.StatusConflict, "already_exists", "that resource already exists")
			return
		case "23514": // check_violation (insufficient funds, key reuse, invalid state, ...)
			switch {
			case strings.Contains(msg, "insufficient"):
				// Crafted, caller-meaningful message (e.g. clawback / funds) — safe to surface.
				writeError(w, http.StatusUnprocessableEntity, "insufficient_funds", msg)
			case strings.Contains(msg, "idempotency key"):
				writeError(w, http.StatusUnprocessableEntity, "idempotency_key_conflict", "this request was already submitted with different parameters")
			case strings.Contains(msg, "account limit"):
				// self-open cap: a conflict with current state, not bad input
				writeError(w, http.StatusConflict, "account_limit", msg)
			case strings.Contains(msg, "already handled"):
				// maker-checker double-resolve (approve/reject raced or repeated)
				writeError(w, http.StatusConflict, "invalid_state", msg)
			case strings.Contains(msg, "invitation limit"):
				// invitation budget exhausted — a conflict with current state
				writeError(w, http.StatusConflict, "invitation_limit", msg)
			case strings.Contains(msg, "invitation code"):
				// consumed / expired / (empty) invitation code — invalid state
				writeError(w, http.StatusConflict, "invalid_state", msg)
			case strings.Contains(msg, "blocked"):
				// Rec 22 fraud gate: a warning rule refused the payment
				// ('payment blocked: <headline>'). Crafted, caller-meaningful.
				writeError(w, http.StatusUnprocessableEntity, "payment_blocked", msg)
			case strings.Contains(msg, "acknowledgement"):
				// Rec 22: a required fraud-warning acknowledgement is missing / too
				// fresh / expired — re-acknowledge (respecting cooling-off) and retry.
				writeError(w, http.StatusConflict, "ack_required", msg)
			default: // raw constraint trip — don't leak the constraint name
				writeError(w, http.StatusUnprocessableEntity, "unprocessable", "the request could not be processed")
			}
			return
		case "23001": // restrict_violation (append-only ledger / balance tamper guard)
			writeError(w, http.StatusConflict, "immutable", "this resource cannot be modified")
			return
		case "55006": // object_in_use (idempotent request still in progress)
			writeError(w, http.StatusConflict, "in_progress", "a previous request is still being processed")
			return
		case "53400": // configuration_limit_exceeded (verification resend cooldown)
			writeError(w, http.StatusTooManyRequests, "rate_limited", "please wait before requesting another code")
			return
		case "28000", "28P01": // refresh-token reuse/expiry/unknown -> re-authenticate
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in again")
			return
		case "42501": // insufficient_privilege: caller doesn't own the debit account
			writeError(w, http.StatusForbidden, "forbidden", "you do not have access to this resource")
			return
		case "22P02": // invalid_text_representation (e.g. a malformed UUID reached the DB)
			writeError(w, http.StatusBadRequest, "bad_request", "invalid input")
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

	// Catch-all: an error we never mapped is a genuine internal fault (a missing
	// function, a permissions glitch, a pool error, an unanticipated SQLSTATE). The
	// client gets a curated, leak-free body, but we MUST log the full underlying
	// error server-side — correlated by request_id via s.logFor — or operators are
	// blind to real 500s. Surface the SQLSTATE separately when we have one.
	pgCode := ""
	if pg != nil {
		pgCode = pg.Code
	}
	s.logFor(r.Context()).Error("unmapped db error -> 500", "err", err, "sqlstate", pgCode)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// dbErrorMessage extracts a human-readable message from a DB error, for console
// flash messages (the JSON API uses mapDBError instead). It is a pure mapping; the
// console handlers reach it via s.dbFlash so an unexpected error is also logged.
func dbErrorMessage(err error) string {
	if errors.Is(err, pgx.ErrNoRows) {
		return "not found"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		// Echo the Postgres message ONLY for crafted SQLSTATEs whose text is a
		// developer-authored, operator-safe business message. Any other PgError
		// (a raw constraint name, a generic engine error) gets a curated message so
		// schema internals don't leak into the console flash banner; the full error
		// is already logged by dbFlash.
		if craftedFlashCodes[pg.Code] {
			return pg.Message
		}
		return "database error"
	}
	return "internal error"
}

// craftedFlashCodes is the whitelist of SQLSTATEs whose raw Postgres message is a
// developer-authored, operator-safe string (mirrors the crafted branches in
// mapDBError). Every other PgError is curated to a generic "database error".
var craftedFlashCodes = map[string]bool{
	"P0001": true, // generic RAISE EXCEPTION (business message)
	"23514": true, // check_violation (insufficient funds, key reuse, ...)
	"55006": true, // object_in_use (request still in progress)
	"23001": true, // restrict_violation (append-only guards)
	"28000": true, // invalid_authorization_specification
	"28P01": true, // invalid_password
	"42501": true, // insufficient_privilege
	"53400": true, // configuration_limit_exceeded (resend cooldown)
}

// dbFlash is the console-flash counterpart to mapDBError: it returns the
// human-readable message for the flash banner, but — like the 500 path in
// mapDBError — logs the FULL underlying error server-side (correlated by
// request_id) when it is an unexpected internal fault, so a console operator's
// "internal error" banner isn't a dead end in the logs either. A recognised
// pgx.ErrNoRows / business PgError is an expected outcome and is not logged.
func (s *Server) dbFlash(r *http.Request, err error) string {
	if !errors.Is(err, pgx.ErrNoRows) {
		var pg *pgconn.PgError
		if !errors.As(err, &pg) {
			s.logFor(r.Context()).Error("unmapped db error -> console flash", "err", err)
		}
	}
	return dbErrorMessage(err)
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
