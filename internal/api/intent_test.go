package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

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

func transferRowCount(t *testing.T, pg *db.Postgres) int64 {
	t.Helper()
	var n int64
	if err := pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM transfers`).Scan(&n); err != nil {
		t.Fatalf("count transfers: %v", err)
	}
	return n
}
