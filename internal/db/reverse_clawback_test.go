package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// REVERSE-SPENT-FUNDS (migration 00031): reverse_transfer appends an inverse
// 'reversal' transfer that DEBITS the original CREDIT account. If the recipient
// still holds the money the clawback succeeds and the ledger nets to zero; if
// they have already SPENT it (balance_minor < amount), the up-front funds check
// RAISEs a clear check_violation (23514) whose message contains "insufficient"
// — NOT the raw "accounts_check" constraint name — and nothing is written.

// reverseTransfer invokes reverse_transfer via a raw SELECT so we observe the
// real SQLSTATE/message instead of a guessed Go wrapper.
func reverseTransfer(t *testing.T, pg *Postgres, transferID uuid.UUID) (uuid.UUID, error) {
	t.Helper()
	var revID uuid.UUID
	err := pg.Pool.QueryRow(context.Background(),
		`SELECT reverse_transfer($1,$2,$3)`,
		transferID, uuid.NewString(), "clawback").Scan(&revID)
	return revID, err
}

// (a) Recipient STILL HOLDS the funds: the clawback succeeds, money returns to
// the debit account, the recipient is zeroed, the original is marked reversed,
// and the global double-entry invariant still holds.
func TestReverseClawbackSucceedsWhenRecipientStillHoldsFunds(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	res, err := pg.Transfer(ctx, uuid.NewString(), a, b, 4_000, "to claw back", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("transfer a->b: %v", err)
	}
	if res.Status != sqlc.TransferStatusPosted {
		t.Fatalf("want posted, got %q", res.Status)
	}
	if lb, _ := balance(t, pg, b); lb != 4_000 {
		t.Fatalf("recipient balance = %d, want 4000 (still holds the funds)", lb)
	}

	if _, err := reverseTransfer(t, pg, res.TransferID); err != nil {
		t.Fatalf("reverse (recipient still holds): %v", err)
	}

	// Money returns to a; b is zeroed; original is reversed (never edited).
	if lb, _ := balance(t, pg, a); lb != 10_000 {
		t.Errorf("debit restored = %d, want 10000", lb)
	}
	if lb, _ := balance(t, pg, b); lb != 0 {
		t.Errorf("recipient after clawback = %d, want 0", lb)
	}
	var origStatus string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status FROM transfers WHERE id = $1`, res.TransferID).Scan(&origStatus); err != nil {
		t.Fatalf("read original status: %v", err)
	}
	if origStatus != "reversed" {
		t.Errorf("original status = %q, want reversed", origStatus)
	}
	reconcileClean(t, pg) // ledger nets to zero
}

// (b) Recipient has SPENT the funds (balance_minor < the amount to claw back):
// the reversal must fail with a check_violation (23514) whose message contains
// "insufficient" (so mapDBError routes it to insufficient_funds / 422), and it
// must leave the ledger UNCHANGED — the function is atomic, so no partial
// reversal leg is written and the original stays 'posted'.
func TestReverseClawbackRejectedWhenRecipientHasSpentFunds(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	c := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	// a -> b (the transfer we will try to claw back).
	res, err := pg.Transfer(ctx, uuid.NewString(), a, b, 4_000, "to claw back", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("transfer a->b: %v", err)
	}
	if res.Status != sqlc.TransferStatusPosted {
		t.Fatalf("want posted, got %q", res.Status)
	}

	// b spends most of it onward to c, dropping b's balance below 4000.
	if _, err := pg.Transfer(ctx, uuid.NewString(), b, c, 3_500, "mule cash-out", sqlc.TransferKindTransfer); err != nil {
		t.Fatalf("transfer b->c (spend): %v", err)
	}
	if lb, _ := balance(t, pg, b); lb != 500 {
		t.Fatalf("recipient balance = %d, want 500 (spent down below 4000)", lb)
	}

	// Snapshot the ledger so we can prove nothing changed on failure.
	ledgerBefore := ledgerRowCount(t, pg)

	_, err = reverseTransfer(t, pg, res.TransferID)
	if err == nil {
		t.Fatal("reversing a transfer whose recipient has spent the funds must be rejected")
	}
	if got := sqlstate(err); got != "23514" {
		t.Errorf("clawback-over-spent SQLSTATE = %q, want 23514 (check_violation)", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "insufficient") {
		t.Errorf("error message must contain %q (for mapDBError -> insufficient_funds); got %q", "insufficient", msg)
	}
	if strings.Contains(msg, "accounts_check") {
		t.Errorf("error message must NOT leak the raw constraint name; got %q", msg)
	}

	// Atomic: no partial reversal leg, balances untouched, original still posted.
	if ledgerAfter := ledgerRowCount(t, pg); ledgerAfter != ledgerBefore {
		t.Errorf("ledger row count changed on failed reversal: before=%d after=%d", ledgerBefore, ledgerAfter)
	}
	if lb, _ := balance(t, pg, a); lb != 6_000 {
		t.Errorf("debit account moved on failed reversal = %d, want 6000", lb)
	}
	if lb, _ := balance(t, pg, b); lb != 500 {
		t.Errorf("recipient moved on failed reversal = %d, want 500", lb)
	}
	if lb, _ := balance(t, pg, c); lb != 3_500 {
		t.Errorf("third party moved on failed reversal = %d, want 3500", lb)
	}
	var origStatus string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status FROM transfers WHERE id = $1`, res.TransferID).Scan(&origStatus); err != nil {
		t.Fatalf("read original status: %v", err)
	}
	if origStatus != "posted" {
		t.Errorf("original status after failed reversal = %q, want posted (unchanged)", origStatus)
	}
	reconcileClean(t, pg) // ledger invariants intact — no orphan/unbalanced legs
}

func ledgerRowCount(t *testing.T, pg *Postgres) int64 {
	t.Helper()
	var n int64
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_entries`).Scan(&n); err != nil {
		t.Fatalf("count ledger_entries: %v", err)
	}
	return n
}
