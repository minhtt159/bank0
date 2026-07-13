package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Rec 25 AML screening: a watchlist name-hit on either party parks the payment
// 'under_review' (operator-only), releasable only via approve/reject.

// screeningReqID returns the pending screening_hold admin_actions row for a transfer.
func screeningReqID(t *testing.T, pg *Postgres, tid uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT id FROM admin_actions WHERE action = 'screening_hold' AND target_id = $1 AND approved_by IS NULL`,
		tid).Scan(&id); err != nil {
		t.Fatalf("screening request id: %v", err)
	}
	return id
}

func TestScreeningFlow(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	operator := mkCustomer(t, pg)
	normal := mkAccount(t, pg, mkCustomer(t, pg))

	watched := mkNamedCustomer(t, pg, "Sanctioned Party "+uniqHex(8))
	var watchName string
	_ = pg.Pool.QueryRow(ctx, `SELECT full_name FROM users WHERE id = $1`, watched).Scan(&watchName)
	addWatchlist(t, pg, "%"+watchName+"%")

	mule := mkAccount(t, pg, watched)
	mule2 := mkAccount(t, pg, watched)
	mule3 := mkAccount(t, pg, watched)
	watchedDebit := mkAccount(t, pg, watched)
	fund(t, pg, aAcct, 1_000_000)
	fund(t, pg, watchedDebit, 100_000) // deposit is sentinel -> not screened

	// (1) Creditor match: alice -> mule parks under_review.
	tid, st, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, mule, 5_000)
	if err != nil {
		t.Fatalf("alice->mule: %v", err)
	}
	if st != sqlc.TransferStatusUnderReview {
		t.Fatalf("creditor-match status = %s, want under_review", st)
	}
	// approve by a different user -> posted.
	req := screeningReqID(t, pg, tid)
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, req, operator); err != nil {
		t.Fatalf("approve screening: %v", err)
	}
	if transferStatus(t, pg, tid) != sqlc.TransferStatusPosted {
		t.Errorf("after approve status = %s, want posted", transferStatus(t, pg, tid))
	}
	if led, _ := balance(t, pg, mule); led != 5_000 {
		t.Errorf("approved screening credit = %d, want 5000", led)
	}
	// double-approve -> 23514 already handled.
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, req, operator); sqlstate(err) != "23514" {
		t.Errorf("double-approve sqlstate = %q, want 23514", sqlstate(err))
	}

	// (2) Debtor match: the watched user paying out is also screened.
	dtid, dst, _, err := clientXfer(ctx, pg, watched, uuid.NewString(), watchedDebit, normal, 1_000)
	if err != nil {
		t.Fatalf("watched->normal: %v", err)
	}
	if dst != sqlc.TransferStatusUnderReview {
		t.Errorf("debtor-match status = %s, want under_review", dst)
	}

	// (3) Reject releases the hold and cancels the transfer.
	rtid, rst, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, mule2, 7_000)
	if err != nil || rst != sqlc.TransferStatusUnderReview {
		t.Fatalf("stage reject target: st=%s err=%v", rst, err)
	}
	if _, avail := balance(t, pg, aAcct); avail != 1_000_000-5_000-7_000 {
		t.Errorf("available before reject = %d, want %d", avail, 1_000_000-5_000-7_000)
	}
	rreq := screeningReqID(t, pg, rtid)
	if _, err := pg.Pool.Exec(ctx, `SELECT reject_request($1,$2,$3)`, rreq, operator, "screening declined"); err != nil {
		t.Fatalf("reject screening: %v", err)
	}
	if transferStatus(t, pg, rtid) != sqlc.TransferStatusCanceled {
		t.Errorf("rejected status = %s, want canceled", transferStatus(t, pg, rtid))
	}
	if _, avail := balance(t, pg, aAcct); avail != 1_000_000-5_000 {
		t.Errorf("available after reject = %d, want %d (hold released)", avail, 1_000_000-5_000)
	}

	// (4) The customer can neither confirm nor cancel a transfer under review.
	gtid, gst, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, mule3, 3_000)
	if err != nil || gst != sqlc.TransferStatusUnderReview {
		t.Fatalf("stage guard target: st=%s err=%v", gst, err)
	}
	if _, err := pg.Queries.ClientConfirmTransfer(ctx, sqlc.ClientConfirmTransferParams{CallerSubject: alice, ID: gtid}); sqlstate(err) != "P0001" {
		t.Errorf("confirm-under_review sqlstate = %q, want P0001", sqlstate(err))
	}
	if _, err := pg.Queries.ClientCancelTransfer(ctx, sqlc.ClientCancelTransferParams{CallerSubject: alice, ID: gtid, Reason: "x"}); err == nil || sqlstate(err) != "P0001" {
		t.Errorf("cancel-under_review sqlstate = %q, want P0001", sqlstate(err))
	}

	// (5) Sentinel deposits are never screened, even to a watchlisted account.
	depID, err := pg.Queries.Deposit(ctx, sqlc.DepositParams{
		IdempotencyKey: uuid.NewString(), AccountID: mule, AmountMinor: 2_000, Description: "sentinel deposit",
	})
	if err != nil {
		t.Fatalf("deposit to watched account: %v", err)
	}
	if transferStatus(t, pg, depID) != sqlc.TransferStatusPosted {
		t.Errorf("sentinel deposit to watched acct status = %s, want posted (never screened)", transferStatus(t, pg, depID))
	}

	// Clean up the leftover parked rows so a later global reconcile stays clean.
	for _, id := range []uuid.UUID{dtid, gtid} {
		if _, err := pg.Pool.Exec(ctx, `SELECT cancel_transfer($1, 'cleanup')`, id); err != nil {
			t.Fatalf("cleanup cancel %s: %v", id, err)
		}
	}
	reconcileClean(t, pg)
}

// TestScreeningNeverAutoReleases: the sweep CANCELS a lapsed under_review transfer;
// it must never post it (the safe direction — no money moves on a timed-out review).
func TestScreeningNeverAutoReleases(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	watched := mkNamedCustomer(t, pg, "Lapsed Review "+uniqHex(8))
	var watchName string
	_ = pg.Pool.QueryRow(ctx, `SELECT full_name FROM users WHERE id = $1`, watched).Scan(&watchName)
	addWatchlist(t, pg, "%"+watchName+"%")
	mule := mkAccount(t, pg, watched)
	fund(t, pg, aAcct, 100_000)

	tid, st, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, mule, 4_000)
	if err != nil || st != sqlc.TransferStatusUnderReview {
		t.Fatalf("stage under_review: st=%s err=%v", st, err)
	}

	if _, err := pg.Pool.Exec(ctx,
		`UPDATE holds SET expires_at = now() - interval '1 hour' WHERE transfer_id = $1 AND status = 'active'`, tid); err != nil {
		t.Fatalf("backdate hold: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT expire_holds()`); err != nil {
		t.Fatalf("expire_holds: %v", err)
	}

	got, _ := pg.Queries.GetTransfer(ctx, tid)
	if got.Status != sqlc.TransferStatusCanceled {
		t.Errorf("swept under_review status = %s, want canceled (never posted)", got.Status)
	}
	if got.FailureReason == nil || *got.FailureReason != "review window expired" {
		t.Errorf("failure_reason = %v, want 'review window expired'", got.FailureReason)
	}
	// No money moved; funds released.
	if led, avail := balance(t, pg, aAcct); led != 100_000 || avail != 100_000 {
		t.Errorf("after sweep ledger=%d available=%d, want 100000/100000", led, avail)
	}
	if led, _ := balance(t, pg, mule); led != 0 {
		t.Errorf("mule credited on a timed-out review = %d, want 0", led)
	}
	reconcileClean(t, pg)
}

// TestReconcileCatchesMissingHold: a parked transfer whose active hold vanished is
// caught by reconcile()'s I5 missing_hold invariant.
func TestReconcileCatchesMissingHold(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bAcct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)
	addWarningRule(t, pg, "first_payment_to_payee", "", "review", false, 0, 100)

	tid, st, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, bAcct, 5_000)
	if err != nil || st != sqlc.TransferStatusHeld {
		t.Fatalf("stage held: st=%s err=%v", st, err)
	}

	// Release the hold out from under the still-held transfer (the trigger keeps
	// held_minor correct, so ONLY missing_hold should fire).
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE holds SET status = 'released', released_at = now() WHERE transfer_id = $1 AND status = 'active'`, tid); err != nil {
		t.Fatalf("release hold: %v", err)
	}

	issues, err := pg.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	found := false
	for _, iss := range issues {
		if iss.CheckName == "missing_hold" && strings.Contains(iss.Detail, tid.String()) {
			found = true
		}
		if iss.CheckName == "held_drift" && strings.Contains(iss.Detail, aAcct.String()) {
			t.Errorf("unexpected held_drift alongside missing_hold: %s", iss.Detail)
		}
	}
	if !found {
		t.Errorf("reconcile did not report missing_hold for %s; issues=%+v", tid, issues)
	}

	// Restore a clean global state for later tests.
	if _, err := pg.Pool.Exec(ctx, `SELECT cancel_transfer($1, 'cleanup')`, tid); err != nil {
		t.Fatalf("cleanup cancel: %v", err)
	}
	reconcileClean(t, pg)
}
