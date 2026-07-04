package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// These exercise the maintenance/dispute/beneficiary/guided/reconcile PL/pgSQL
// (migrations 00009, 00020, 00016, 00019). DB functions are invoked via raw
// pg.Pool SELECTs so we assert on real SQLSTATEs, not guessed Go wrappers.

// ─── expire_holds (00009) ────────────────────────────────────────────────────

// A pending transfer holds funds (available drops). Once the hold is past its
// expiry, expire_holds() flips the hold -> 'expired' and the transfer -> 'failed',
// and the reserved availability is restored.
func TestExpireHoldsFailsPendingTransferAndRestoresAvailable(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	// request_transfer creates a PENDING transfer + an active hold reserving 4000.
	var tid uuid.UUID
	var status string
	var wasReplay bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay
		   FROM request_transfer($1,$2,$3,$4,$5,'transfer')`,
		uuid.NewString(), a, b, int64(4_000), "pending").Scan(&tid, &status, &wasReplay); err != nil {
		t.Fatalf("request_transfer: %v", err)
	}
	if status != "pending" {
		t.Fatalf("want pending transfer, got %q", status)
	}
	if led, avail := balance(t, pg, a); led != 10_000 || avail != 6_000 {
		t.Fatalf("before expiry: ledger=%d available=%d, want 10000/6000", led, avail)
	}

	// Backdate the hold so it is past expiry (holds has no guard trigger).
	ct, err := pg.Pool.Exec(ctx,
		`UPDATE holds SET expires_at = now() - interval '1 hour'
		  WHERE transfer_id = $1 AND status = 'active'`, tid)
	if err != nil {
		t.Fatalf("backdate hold: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("expected to backdate exactly one active hold, affected=%d", ct.RowsAffected())
	}

	if _, err := pg.Pool.Exec(ctx, `SELECT expire_holds()`); err != nil {
		t.Fatalf("expire_holds: %v", err)
	}

	// Hold is now expired.
	var holdStatus string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status FROM holds WHERE transfer_id = $1`, tid).Scan(&holdStatus); err != nil {
		t.Fatalf("read hold status: %v", err)
	}
	if holdStatus != "expired" {
		t.Errorf("hold status = %q, want expired", holdStatus)
	}

	// Transfer is now failed.
	var tStatus string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status FROM transfers WHERE id = $1`, tid).Scan(&tStatus); err != nil {
		t.Fatalf("read transfer status: %v", err)
	}
	if tStatus != "failed" {
		t.Errorf("transfer status = %q, want failed", tStatus)
	}

	// Availability restored: no active hold reserving funds.
	if led, avail := balance(t, pg, a); led != 10_000 || avail != 10_000 {
		t.Errorf("after expiry: ledger=%d available=%d, want 10000/10000", led, avail)
	}
}

// expire_holds() counts HOLDS expired, not transfers flipped to 'failed': a hold
// whose transfer already left 'pending' is still expired and still counted.
func TestExpireHoldsCountsHoldsNotTransfers(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	var tid uuid.UUID
	var status string
	var wasReplay bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay
		   FROM request_transfer($1,$2,$3,$4,$5,'transfer')`,
		uuid.NewString(), a, b, int64(4_000), "pending").Scan(&tid, &status, &wasReplay); err != nil {
		t.Fatalf("request_transfer: %v", err)
	}

	// The transfer leaves 'pending' while its hold stays active, then the hold
	// passes expiry. (Direct UPDATE: transfers has no guard trigger.)
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE transfers SET status = 'failed', failure_reason = 'test' WHERE id = $1`, tid); err != nil {
		t.Fatalf("fail transfer: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE holds SET expires_at = now() - interval '1 hour'
		  WHERE transfer_id = $1 AND status = 'active'`, tid); err != nil {
		t.Fatalf("backdate hold: %v", err)
	}

	// Old body counted the transfers UPDATE (0 rows here); the count must reflect
	// the expired hold. >= 1 rather than == 1: the count is global, and the api
	// suite shares this database.
	var n int
	if err := pg.Pool.QueryRow(ctx, `SELECT expire_holds()`).Scan(&n); err != nil {
		t.Fatalf("expire_holds: %v", err)
	}
	if n < 1 {
		t.Errorf("expire_holds() = %d, want >= 1 (the expired hold)", n)
	}

	var holdStatus string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status FROM holds WHERE transfer_id = $1`, tid).Scan(&holdStatus); err != nil {
		t.Fatalf("read hold status: %v", err)
	}
	if holdStatus != "expired" {
		t.Errorf("hold status = %q, want expired", holdStatus)
	}
}

// cleanup_idempotency_keys reaps committed in_progress orphans (>15 min) as well
// as expired completed keys. Orphans are unreachable via the single-txn claim
// paths today, so the test manufactures one directly.
func TestCleanupReapsStaleInProgressKeys(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	stale, fresh := "stale-"+uuid.NewString(), "fresh-"+uuid.NewString()
	if _, err := pg.Pool.Exec(ctx,
		`INSERT INTO idempotency_keys (key, scope, request_hash, status, created_at)
		 VALUES ($1, 'transfer', 'h', 'in_progress', now() - interval '16 minutes'),
		        ($2, 'transfer', 'h', 'in_progress', now())`, stale, fresh); err != nil {
		t.Fatalf("seed keys: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT cleanup_idempotency_keys()`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var staleLeft, freshLeft int
	_ = pg.Pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, stale).Scan(&staleLeft)
	_ = pg.Pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, fresh).Scan(&freshLeft)
	if staleLeft != 0 {
		t.Error("stale in_progress key must be reaped")
	}
	if freshLeft != 1 {
		t.Error("fresh in_progress key must survive (a live claim window)")
	}
}

// ─── disputes (00020) ────────────────────────────────────────────────────────

// postedTransfer creates a posted transfer a->b (a owned by ownerA, b by ownerB).
func postedTransfer(t *testing.T, pg *Postgres, a, b uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var tid uuid.UUID
	var status string
	var wasReplay bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay FROM transfer($1,$2,$3,$4,$5,'transfer')`,
		uuid.NewString(), a, b, int64(3_000), "for dispute").Scan(&tid, &status, &wasReplay); err != nil {
		t.Fatalf("transfer (post): %v", err)
	}
	if status != "posted" {
		t.Fatalf("want posted transfer, got %q", status)
	}
	return tid
}

// raise_dispute by a party (the debit owner) succeeds; by a non-party it hides
// existence (P0001 -> 404); a second OPEN dispute by the same party on the same
// transfer hits the partial unique index (23505 -> 409).
func TestRaiseDisputePartyChecksAndUniqueOpen(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	ownerA := mkCustomer(t, pg)
	ownerB := mkCustomer(t, pg)
	stranger := mkCustomer(t, pg)
	a := mkAccount(t, pg, ownerA)
	b := mkAccount(t, pg, ownerB)
	fund(t, pg, a, 10_000)
	tid := postedTransfer(t, pg, a, b)

	// Party (debit owner) may raise a dispute.
	var did uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT raise_dispute($1,$2,'unrecognised','not me')`, tid, ownerA).Scan(&did); err != nil {
		t.Fatalf("raise_dispute by party: %v", err)
	}
	if did == uuid.Nil {
		t.Fatal("raise_dispute returned nil dispute id")
	}

	// Non-party: existence is hidden -> bare RAISE EXCEPTION -> P0001 -> 404.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT raise_dispute($1,$2,'unrecognised','')`, tid, stranger); err == nil {
		t.Fatal("non-party raise_dispute must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Errorf("non-party SQLSTATE = %q, want P0001", got)
	}

	// Second OPEN dispute by the same party on the same transfer -> unique_violation.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT raise_dispute($1,$2,'fraud','again')`, tid, ownerA); err == nil {
		t.Fatal("duplicate open dispute must be rejected")
	} else if got := sqlstate(err); got != "23505" {
		t.Errorf("duplicate dispute SQLSTATE = %q, want 23505", got)
	}
}

// raise_dispute against a still-PENDING (non-settled) transfer must fail with
// check_violation (23514 -> 422): only posted/reversed transfers are disputable.
func TestRaiseDisputeOnPendingTransferRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	ownerA := mkCustomer(t, pg)
	a := mkAccount(t, pg, ownerA)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	// A pending (not posted) transfer.
	var tid uuid.UUID
	var status string
	var wasReplay bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay FROM request_transfer($1,$2,$3,$4,$5,'transfer')`,
		uuid.NewString(), a, b, int64(2_000), "pending").Scan(&tid, &status, &wasReplay); err != nil {
		t.Fatalf("request_transfer: %v", err)
	}
	if status != "pending" {
		t.Fatalf("want pending, got %q", status)
	}

	if _, err := pg.Pool.Exec(ctx,
		`SELECT raise_dispute($1,$2,'unrecognised','')`, tid, ownerA); err == nil {
		t.Fatal("dispute on a pending transfer must be rejected")
	} else if got := sqlstate(err); got != "23514" {
		t.Errorf("pending dispute SQLSTATE = %q, want 23514 (check_violation)", got)
	}
}

// resolve_dispute walks the state machine: open -> under_review -> resolved, then
// resolving the now-terminal dispute again is rejected (P0001 -> 409).
func TestResolveDisputeStateMachine(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	ownerA := mkCustomer(t, pg)
	resolver := mkCustomer(t, pg) // any user id stands in for the operator
	a := mkAccount(t, pg, ownerA)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)
	tid := postedTransfer(t, pg, a, b)

	var did uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT raise_dispute($1,$2,'unrecognised','')`, tid, ownerA).Scan(&did); err != nil {
		t.Fatalf("raise_dispute: %v", err)
	}

	// open -> under_review
	var st string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT resolve_dispute($1,$2,'under_review','looking')`, did, resolver).Scan(&st); err != nil {
		t.Fatalf("open->under_review: %v", err)
	}
	if st != "under_review" {
		t.Errorf("status after first transition = %q, want under_review", st)
	}

	// under_review -> resolved
	if err := pg.Pool.QueryRow(ctx,
		`SELECT resolve_dispute($1,$2,'resolved','reversed it')`, did, resolver).Scan(&st); err != nil {
		t.Fatalf("under_review->resolved: %v", err)
	}
	if st != "resolved" {
		t.Errorf("status after second transition = %q, want resolved", st)
	}

	// resolved is terminal -> transitioning again is rejected (plain RAISE -> P0001).
	if _, err := pg.Pool.Exec(ctx,
		`SELECT resolve_dispute($1,$2,'rejected','')`, did, resolver); err == nil {
		t.Fatal("transitioning a terminal dispute must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Errorf("terminal-transition SQLSTATE = %q, want P0001", got)
	}
}

// ─── beneficiaries (00016) ───────────────────────────────────────────────────

// resolve_account_by_iban returns a row for an active customer account, and
// raises (P0001 -> 404) when the account is frozen (existence hidden).
func TestResolveAccountByIban(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	acct := mkAccount(t, pg, mkCustomer(t, pg))

	var ibanStr string
	if err := pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id = $1`, acct).Scan(&ibanStr); err != nil {
		t.Fatalf("read iban: %v", err)
	}

	var gotID uuid.UUID
	var gotIban, masked string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT account_id, iban, owner_name_masked FROM resolve_account_by_iban($1)`,
		ibanStr).Scan(&gotID, &gotIban, &masked); err != nil {
		t.Fatalf("resolve active account: %v", err)
	}
	if gotID != acct || gotIban != ibanStr {
		t.Errorf("resolved %s/%s, want %s/%s", gotID, gotIban, acct, ibanStr)
	}

	// Freeze the account; resolve must now hide it.
	if _, err := pg.Pool.Exec(ctx, `UPDATE accounts SET status = 'frozen' WHERE id = $1`, acct); err != nil {
		t.Fatalf("freeze account: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT account_id FROM resolve_account_by_iban($1)`, ibanStr); err == nil {
		t.Fatal("resolving a frozen account must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Errorf("frozen-resolve SQLSTATE = %q, want P0001", got)
	}
}

// add_beneficiary rejects saving the caller's OWN account, and a duplicate
// (owner, account) hits the UNIQUE index (23505 -> 409).
func TestAddBeneficiaryRejectsOwnAndDuplicate(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)
	own := mkAccount(t, pg, owner) // the caller's own account

	var ownIban string
	if err := pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id = $1`, own).Scan(&ownIban); err != nil {
		t.Fatalf("read own iban: %v", err)
	}

	// Saving your own account is rejected (plain RAISE -> P0001).
	if _, err := pg.Pool.Exec(ctx,
		`SELECT add_beneficiary($1,$2,$3)`, owner, "myself", ownIban); err == nil {
		t.Fatal("adding own account as beneficiary must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Errorf("own-account SQLSTATE = %q, want P0001", got)
	}

	// A different customer's account can be saved once...
	other := mkAccount(t, pg, mkCustomer(t, pg))
	var otherIban string
	if err := pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id = $1`, other).Scan(&otherIban); err != nil {
		t.Fatalf("read other iban: %v", err)
	}
	var bid uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT add_beneficiary($1,$2,$3)`, owner, "friend", otherIban).Scan(&bid); err != nil {
		t.Fatalf("add_beneficiary first: %v", err)
	}

	// ...but a second add for the same (owner, account) hits the unique index.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT add_beneficiary($1,$2,$3)`, owner, "friend again", otherIban); err == nil {
		t.Fatal("duplicate beneficiary must be rejected")
	} else if got := sqlstate(err); got != "23505" {
		t.Errorf("duplicate-beneficiary SQLSTATE = %q, want 23505", got)
	}
}

// ─── guided suggestion / mule menu (00032) ───────────────────────────────────
// The v2 resolver suggest_transfer_destinations draws only from the active
// guided_scenarios short-list, so it's exercised end-to-end in the API suite
// (internal/api/suggestion_test.go), which owns guided_scenarios writes
// sequentially. A DB-layer test here would race that suite's TRUNCATE on the
// shared test database, so the coverage lives there.

// ─── reconcile drift detection (00009) ───────────────────────────────────────

// reconcile() reports drift. We bypass triggers in a rolled-back tx, introduce a
// single unbalanced ledger row, and assert reconcile() (run in the same tx) sees
// it. The unbalanced leg trips both 'transfer_unbalanced' (I2) and 'global_nonzero'
// (I3), so count(*) must be > 0.
func TestReconcileDetectsDrift(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)
	// A real posted transfer to attach an orphan, unbalanced extra leg to.
	tid := postedTransfer(t, pg, a, b)

	tx, err := pg.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	// Bypass append-only / balance guard triggers for this tx only.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
		t.Fatalf("set replica role: %v", err)
	}

	// Insert one unbalanced ledger leg: the transfer's legs no longer net to zero
	// and the global sum is non-zero.
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_minor, currency, balance_after)
		 VALUES ($1, $2, 'credit', 1, 'EUR', 0)`, tid, a); err != nil {
		t.Fatalf("insert unbalanced leg: %v", err)
	}

	var issues int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconcile()`).Scan(&issues); err != nil {
		t.Fatalf("reconcile in tx: %v", err)
	}
	if issues == 0 {
		t.Errorf("reconcile() found no issues despite injected drift; want > 0")
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}
