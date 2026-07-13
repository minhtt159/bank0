package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Rec 22 fraud/warning decision gate: block, acknowledgement enforcement, the
// read-only evaluate_transfer preflight, and velocity self-exclusion.

func accountIban(t *testing.T, pg *Postgres, acct uuid.UUID) string {
	t.Helper()
	var iban string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(iban, system_code, '') FROM accounts WHERE id = $1`, acct).Scan(&iban); err != nil {
		t.Fatalf("account iban: %v", err)
	}
	return iban
}

func assessReasons(t *testing.T, pg *Postgres, caller, debit, credit uuid.UUID, amount int64, exclude *uuid.UUID) []string {
	t.Helper()
	var band string
	var score int32
	var reasons []string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT risk_band, score, reasons FROM assess_transfer_risk($1,$2,$3,$4,$5)`,
		caller, debit, credit, amount, exclude).Scan(&band, &score, &reasons); err != nil {
		t.Fatalf("assess_transfer_risk: %v", err)
	}
	return reasons
}

func hasReason(rs []string, want string) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// TestBlockRollsBackKeyFully: a 'block' decision RAISEs check_violation and rolls
// back the ENTIRE call — no money moves and the idempotency key is not left claimed.
func TestBlockRollsBackKeyFully(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bAcct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)

	addWarningRule(t, pg, "first_payment_to_payee", "", "block", false, 0, 100)

	key := uuid.NewString()
	_, _, _, err := clientXfer(ctx, pg, alice, key, aAcct, bAcct, 5_000)
	if sqlstate(err) != "23514" {
		t.Fatalf("block sqlstate = %q, want 23514", sqlstate(err))
	}
	if err == nil || !strings.Contains(err.Error(), "payment blocked") {
		t.Errorf("block message = %v, want to contain 'payment blocked'", err)
	}

	// The idempotency key was rolled back with everything else (not left claimed).
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE owner_id = $1 AND key = $2`, alice, key).Scan(&n); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if n != 0 {
		t.Errorf("blocked call left %d idempotency key(s), want 0 (full rollback)", n)
	}
	// No money moved.
	if led, avail := balance(t, pg, aAcct); led != 100_000 || avail != 100_000 {
		t.Errorf("blocked call moved funds: ledger=%d available=%d, want 100000/100000", led, avail)
	}
	reconcileClean(t, pg)
}

// TestAckEnforcement: a required-ack rule demands a matching, correctly-aged
// warning_acks row. Missing / too-fresh / too-old / mismatched -> 23514; a backdated
// ack inside the window lets the payment through.
func TestAckEnforcement(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	fund(t, pg, aAcct, 1_000_000)

	const cool = 60
	const amt = 5_000
	addWarningRule(t, pg, "first_payment_to_payee", "", "warn", true, cool, 100)

	// Distinct fresh payees so each attempt is a first payment and only its own ack
	// (matched by counterparty IBAN) can satisfy it.
	dst := func() (uuid.UUID, string) {
		a := mkAccount(t, pg, mkCustomer(t, pg))
		return a, accountIban(t, pg, a)
	}
	insertAck := func(iban string, amount int64, ageSecs int) {
		if _, err := pg.Pool.Exec(ctx,
			`INSERT INTO warning_acks (user_id, category, reason_code, acknowledged,
			     debit_account_id, counterparty_iban, amount_minor, device, created_at)
			 VALUES ($1, 'risk_warning', '', TRUE, $2, $3, $4, '', now() - make_interval(secs => $5))`,
			alice, aAcct, iban, amount, ageSecs); err != nil {
			t.Fatalf("insert ack: %v", err)
		}
	}

	// Missing ack -> 23514.
	miss, _ := dst()
	if _, _, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, miss, amt); sqlstate(err) != "23514" {
		t.Errorf("missing-ack sqlstate = %q, want 23514", sqlstate(err))
	}

	// Too fresh (not yet aged past cooling-off) -> 23514.
	fresh, freshIban := dst()
	insertAck(freshIban, amt, 0)
	if _, _, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, fresh, amt); sqlstate(err) != "23514" {
		t.Errorf("too-fresh-ack sqlstate = %q, want 23514", sqlstate(err))
	}

	// Too old (past cooling-off + 30 min) -> 23514.
	old, oldIban := dst()
	insertAck(oldIban, amt, cool+40*60)
	if _, _, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, old, amt); sqlstate(err) != "23514" {
		t.Errorf("too-old-ack sqlstate = %q, want 23514", sqlstate(err))
	}

	// Mismatched amount -> 23514.
	mm, mmIban := dst()
	insertAck(mmIban, amt+1, cool+5)
	if _, _, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, mm, amt); sqlstate(err) != "23514" {
		t.Errorf("mismatch-ack sqlstate = %q, want 23514", sqlstate(err))
	}

	// Correctly backdated inside the window -> the payment posts (warn, not review).
	ok, okIban := dst()
	insertAck(okIban, amt, cool+5)
	_, st, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, ok, amt)
	if err != nil {
		t.Fatalf("valid-ack transfer: %v", err)
	}
	if st != sqlc.TransferStatusPosted {
		t.Errorf("valid-ack status = %s, want posted", st)
	}
	reconcileClean(t, pg)
}

// TestEvaluateReadOnlyPrecedenceAndOwnership: evaluate_transfer never writes,
// enforces debit ownership (42501), and collapses matches by precedence
// block > review > step_up > warn > allow.
func TestEvaluateReadOnlyPrecedenceAndOwnership(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bob := mkCustomer(t, pg)
	bAcct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 1_000_000)

	// Foreign debit -> 42501.
	var dummy string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT decision FROM evaluate_transfer($1,$2,$3,$4)`, bob, aAcct, bAcct, int64(1_000)).Scan(&dummy); sqlstate(err) != "42501" {
		t.Errorf("foreign-debit evaluate sqlstate = %q, want 42501", sqlstate(err))
	}

	// Read-only: no transfer/hold/ledger rows created by a run.
	beforeT := tableCount(t, pg, "transfers")
	beforeH := tableCount(t, pg, "holds")
	beforeL := tableCount(t, pg, "ledger_entries")

	// (c) No rule: a first payment is step_up (the axis fires on an unknown payee).
	e, err := pg.EvaluateTransfer(ctx, alice, aAcct, bAcct, 1_000, 0)
	if err != nil {
		t.Fatalf("evaluate (no rule): %v", err)
	}
	if e.Decision != "step_up" {
		t.Errorf("no-rule first payment decision = %q, want step_up", e.Decision)
	}

	// (b) A review rule beats the step_up axis.
	rev := addWarningRule(t, pg, "first_payment_to_payee", "", "review", false, 0, 50)
	if e, _ := pg.EvaluateTransfer(ctx, alice, aAcct, bAcct, 1_000, 0); e.Decision != "review" {
		t.Errorf("review-over-stepup decision = %q, want review", e.Decision)
	}

	// (a) A block rule beats review.
	addWarningRule(t, pg, "first_payment_to_payee", "", "block", false, 0, 100)
	if e, _ := pg.EvaluateTransfer(ctx, alice, aAcct, bAcct, 1_000, 0); e.Decision != "block" {
		t.Errorf("block-over-review decision = %q, want block", e.Decision)
	}
	_ = rev

	if tableCount(t, pg, "transfers") != beforeT ||
		tableCount(t, pg, "holds") != beforeH ||
		tableCount(t, pg, "ledger_entries") != beforeL {
		t.Error("evaluate_transfer must be read-only (row counts changed)")
	}
}

func tableCount(t *testing.T, pg *Postgres, table string) int64 {
	t.Helper()
	var n int64
	if err := pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestVelocityExcludesSelf: the submit-time gate excludes the just-inserted pending
// transfer from its own velocity math, so intent (before insert) and submit (after)
// see the SAME count at a boundary. With 9 prior debits, the 10th must NOT trip a
// velocity_count_24h rule; the reason only appears once all 10 are counted.
func TestVelocityExcludesSelf(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	sink := mkAccount(t, pg, mkCustomer(t, pg))
	bob := mkAccount(t, pg, mkCustomer(t, pg))
	bob2 := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 1_000_000)

	// 9 prior posted debits from alice (sentinel path, no gates).
	for i := 0; i < 9; i++ {
		if _, err := testTransfer(ctx, pg, uuid.NewString(), aAcct, sink, 100, "v", sqlc.TransferKindTransfer); err != nil {
			t.Fatalf("prior debit %d: %v", i, err)
		}
	}

	// Intent (no transfer yet): count = 9 -> velocity_count_24h absent.
	if rs := assessReasons(t, pg, alice, aAcct, bob, 100, nil); hasReason(rs, "velocity_count_24h") {
		t.Fatalf("intent (9 prior) unexpectedly has velocity_count_24h: %v", rs)
	}

	// A review rule keyed on velocity_count_24h.
	addWarningRule(t, pg, "velocity_count_24h", "", "review", false, 0, 100)

	// Submit the 10th: the gate excludes self -> count stays 9 -> rule doesn't fire.
	_, st, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, bob, 100)
	if err != nil {
		t.Fatalf("10th transfer: %v", err)
	}
	if st == sqlc.TransferStatusHeld || st == sqlc.TransferStatusUnderReview {
		t.Errorf("10th transfer parked (%s); velocity gate double-counted the transfer itself", st)
	}

	// Now all 10 are posted: WITHOUT exclusion the reason DOES fire — proving the
	// boundary the self-exclusion straddles is real, not merely absent.
	if rs := assessReasons(t, pg, alice, aAcct, bob2, 100, nil); !hasReason(rs, "velocity_count_24h") {
		t.Errorf("after 10 debits velocity_count_24h absent: %v", rs)
	}
	reconcileClean(t, pg)
}
