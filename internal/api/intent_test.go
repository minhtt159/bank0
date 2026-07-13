package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// clearGates truncates the (globally-shared) fraud/AML rule tables and registers a
// cleanup to truncate them again after the test, so a rule inserted by one HTTP test
// never leaks into another (the api tests share one migrated DB). Sequential test
// execution (none of these use t.Parallel) makes this reliable.
func clearGates(t *testing.T, pg *db.Postgres) {
	t.Helper()
	trunc := func() {
		if _, err := pg.Pool.Exec(context.Background(), `TRUNCATE warning_rules, watchlist_entries`); err != nil {
			t.Fatalf("truncate gate tables: %v", err)
		}
	}
	trunc()
	t.Cleanup(trunc)
}

// addWarningRule inserts an active warning_rules row and returns its id. Shared by
// the intent + held-flow HTTP tests (Wave-2a). Kept out of the shared helper files
// (owned by other test suites) per the wave file split.
func addWarningRule(t *testing.T, pg *db.Postgres, matchReason, matchBand, category, decision string, requiredAck bool, coolingOff, priority int) uuid.UUID {
	t.Helper()
	var mr, mb any
	if matchReason != "" {
		mr = matchReason
	}
	if matchBand != "" {
		mb = matchBand
	}
	var id uuid.UUID
	if err := pg.Pool.QueryRow(context.Background(),
		`INSERT INTO warning_rules (match_reason_code, match_min_band, category, headline, body,
		     severity, decision, required_ack, cooling_off_seconds, priority)
		 VALUES ($1, $2, $3, $4, $5, 'warning', $6, $7, $8, $9) RETURNING id`,
		mr, mb, category, "Please double-check this payment",
		"We think this payment may be risky. Only continue if you trust the recipient.",
		decision, requiredAck, coolingOff, priority).Scan(&id); err != nil {
		t.Fatalf("insert warning_rule: %v", err)
	}
	return id
}

// addWatchlistEntry inserts an active AML watchlist pattern and returns its id.
func addWatchlistEntry(t *testing.T, pg *db.Postgres, pattern, reason string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pg.Pool.QueryRow(context.Background(),
		`INSERT INTO watchlist_entries (pattern, reason) VALUES ($1, $2) RETURNING id`,
		pattern, reason).Scan(&id); err != nil {
		t.Fatalf("insert watchlist_entry: %v", err)
	}
	return id
}

func creditIban(t *testing.T, pg *db.Postgres, acct uuid.UUID) string {
	t.Helper()
	var iban string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(iban, system_code, '') FROM accounts WHERE id = $1`, acct).Scan(&iban); err != nil {
		t.Fatalf("credit iban: %v", err)
	}
	return iban
}

// TestHTTPTransferIntentShape: the preflight returns the decision/band/reason_codes
// shape, moves no money, and NEVER leaks a numeric risk score.
func TestHTTPTransferIntentShape(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)

	tok := clientToken(t, ts, aliceName, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	beforeT := transferRowCount(t, pg)

	r := postJSON(t, ts.URL+"/transfers/intent", auth, map[string]any{
		"debit_account": aliceAcct.String(), "credit_account": bobAcct.String(), "amount_minor": 5_000,
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("intent = %d: %s", r.StatusCode, body(t, r))
	}
	b := body(t, r)
	for _, want := range []string{`"decision"`, `"risk_band"`, `"reason_codes"`} {
		if !strings.Contains(b, want) {
			t.Errorf("intent body missing %s: %s", want, b)
		}
	}
	// A first payment to an unknown payee flags first_payment_to_payee; with no MFA
	// enrolled the step_up axis is downgraded to allow.
	if !strings.Contains(b, "first_payment_to_payee") {
		t.Errorf("intent should surface first_payment_to_payee: %s", b)
	}
	if strings.Contains(b, `"decision":"step_up"`) {
		t.Errorf("no-MFA caller should be downgraded from step_up: %s", b)
	}
	// The numeric score must never leave the DB.
	if strings.Contains(strings.ToLower(b), "score") {
		t.Errorf("intent leaked a score: %s", b)
	}
	// Read-only: no transfer row created.
	if after := transferRowCount(t, pg); after != beforeT {
		t.Errorf("intent created %d transfer rows (must be read-only)", after-beforeT)
	}
}

// TestHTTPTransferIntentForeignDebit: previewing a debit the caller does not own is
// 403 (evaluate_transfer asserts ownership in the DB).
func TestHTTPTransferIntentForeignDebit(t *testing.T) {
	ts, pg := newTestServer(t)
	_, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 1_000_000)
	carolID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	carolAcct := mkAcct(t, pg, carolID, 0)

	tok := clientToken(t, ts, aliceName, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	r := postJSON(t, ts.URL+"/transfers/intent", auth, map[string]any{
		"debit_account": bobAcct.String(), "credit_account": carolAcct.String(), "amount_minor": 1_000,
	})
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("foreign-debit intent = %d, want 403: %s", r.StatusCode, body(t, r))
	}

	// Anonymous -> 401.
	if r := postJSON(t, ts.URL+"/transfers/intent", nil, map[string]any{
		"debit_account": bobAcct.String(), "credit_account": carolAcct.String(), "amount_minor": 1_000,
	}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon intent = %d, want 401", r.StatusCode)
	}
}

// TestHTTPTransferIntentWarning: when a warning rule matches, the preflight returns
// a warning object with its copy + ack policy.
func TestHTTPTransferIntentWarning(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)

	// A first-payment warning that requires acknowledgement.
	ruleID := addWarningRule(t, pg, "first_payment_to_payee", "", "risk_warning", "warn", true, 60, 100)

	tok := clientToken(t, ts, aliceName, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	r := postJSON(t, ts.URL+"/transfers/intent", auth, map[string]any{
		"debit_account": aliceAcct.String(), "credit_account": bobAcct.String(), "amount_minor": 5_000,
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("intent = %d: %s", r.StatusCode, body(t, r))
	}
	b := body(t, r)
	for _, want := range []string{`"warning"`, `"warning_id":"` + ruleID.String() + `"`,
		`"required_ack":true`, `"cooling_off_seconds":60`, `"category":"risk_warning"`} {
		if !strings.Contains(b, want) {
			t.Errorf("intent warning body missing %s: %s", want, b)
		}
	}
}

// TestHTTPIntentSubmitStepUpParity pins the invariant that POST /transfers/intent's
// step-up PREVIEW never diverges from POST /transfers' actual step-up GATE on the
// "new payee" axis. The gate keys "known payee" on a SAVED beneficiary
// (IsKnownPayee); evaluate_transfer must compute its payee axis with the exact same
// predicate — NOT assess_transfer_risk's first_payment_to_payee (prior posted
// transfer). If the two ever drift, a customer is told "no step-up needed" and then
// rejected with 403 (or told to step up for a payment they can already make).
//
// One MFA-enrolled caller, amount below the step-up limit, low risk band — so the
// only axis in play is the payee, isolated across two payees:
//   - carol: SAVED beneficiary the caller has NEVER paid  -> preview allow / gate allows
//   - bob:   PAID before but NEVER saved as a beneficiary -> preview step_up / gate 403
func TestHTTPIntentSubmitStepUpParity(t *testing.T) {
	// StepUpLimitMinor is 5_000 here; all amounts below stay off the value axis.
	ts, pg := newMfaTestServer(t, 5*time.Minute)
	clearGates(t, pg) // no warning/AML rule may perturb the decision — isolate the payee axis

	uid, uname := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, uid, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer) // will be PAID but never saved
	bobAcct := mkAcct(t, pg, bobID, 0)
	carolID, _ := mkUser(t, pg, sqlc.UserRoleCustomer) // will be SAVED but never paid
	carolAcct := mkAcct(t, pg, carolID, 0)

	const amt = int64(500) // below the 5_000 step-up limit

	tok := bearerFor(t, ts.URL, uname, "pw")

	// Create the "previously paid" relationship to bob BEFORE enrolling MFA: an
	// un-enrolled caller is never gated, so this posts and makes bob a paid payee
	// (first_payment_to_payee will be FALSE for bob afterwards) while he stays OUT
	// of the beneficiaries table.
	if r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, amt); r.StatusCode != http.StatusOK {
		t.Fatalf("seed prior payment to bob = %d, want 200: %s", r.StatusCode, body(t, r))
	}

	// Save carol as a beneficiary but NEVER pay her (first_payment_to_payee stays
	// TRUE for carol, yet she is a known payee by the saved-beneficiary predicate).
	carolIban := creditIban(t, pg, carolAcct)
	if _, err := pg.Pool.Exec(context.Background(), `SELECT add_beneficiary($1,'pal',$2)`, uid, carolIban); err != nil {
		t.Fatalf("add beneficiary carol: %v", err)
	}

	// Enroll MFA on the SAME password token: it stays a valid bearer but carries no
	// fresh OTP, so the step-up gate is now reachable for this caller.
	enrollAndConfirm(t, ts.URL, tok)
	auth := map[string]string{"Authorization": "Bearer " + tok}

	intentDecision := func(credit uuid.UUID) (string, string) {
		t.Helper()
		r := postJSON(t, ts.URL+"/transfers/intent", auth, map[string]any{
			"debit_account": aliceAcct.String(), "credit_account": credit.String(), "amount_minor": amt,
		})
		if r.StatusCode != http.StatusOK {
			t.Fatalf("intent = %d: %s", r.StatusCode, body(t, r))
		}
		var out struct {
			Decision     string  `json:"decision"`
			StepUpMethod *string `json:"step_up_method"`
		}
		decodeBody(t, r, &out)
		method := ""
		if out.StepUpMethod != nil {
			method = *out.StepUpMethod
		}
		return out.Decision, method
	}

	// --- Scenario 1: SAVED beneficiary, never paid -> preview & gate BOTH allow. ---
	if dec, method := intentDecision(carolAcct); dec != "allow" || method != "" {
		t.Errorf("saved-payee intent = (%q, method %q), want (allow, no method): "+
			"preview over-promises step-up vs the IsKnownPayee gate", dec, method)
	}
	// The gate must agree: a pwd-only token (no fresh OTP) is NOT rejected.
	r := createXfer(t, ts.URL, tok, aliceAcct, carolAcct, amt)
	if r.StatusCode == http.StatusForbidden {
		t.Fatalf("saved-payee POST /transfers = 403 while intent said allow: "+
			"gate/preview DIVERGENCE on saved beneficiary: %s", body(t, r))
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("saved-payee POST /transfers = %d, want 200: %s", r.StatusCode, body(t, r))
	}

	// --- Scenario 2: PAID before but never saved -> preview & gate BOTH step_up. ---
	if dec, method := intentDecision(bobAcct); dec != "step_up" || method != "otp" {
		t.Errorf("paid-not-saved intent = (%q, method %q), want (step_up, otp): "+
			"preview under-promises step-up vs the IsKnownPayee gate (a prior posted "+
			"transfer must NOT make a payee 'known')", dec, method)
	}
	// The gate must agree: without a fresh OTP the payment is rejected 403 step_up_required.
	r = createXfer(t, ts.URL, tok, aliceAcct, bobAcct, amt)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("paid-not-saved POST /transfers = %d, want 403 while intent said step_up: "+
			"gate/preview DIVERGENCE on paid-but-unsaved payee: %s", r.StatusCode, body(t, r))
	}
	if b := body(t, r); !strings.Contains(b, "step_up_required") {
		t.Errorf("paid-not-saved gate body = %s, want step_up_required", b)
	}
}

func transferRowCount(t *testing.T, pg *db.Postgres) int64 {
	t.Helper()
	var n int64
	if err := pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM transfers`).Scan(&n); err != nil {
		t.Fatalf("count transfers: %v", err)
	}
	return n
}
