package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// iso_status (Rec 20) is the pure enum->ISO-20022 projection. These assert the full
// mapping for all 7 transfer_status values, plus that the projection reads correctly
// across a real lifecycle (posted, reversed original + its reversal, held, canceled,
// expired-hold failed). See db/migrations/00008_transfers.sql and docs/12.

// isoStatus reads iso_status() for one enum value via a direct SELECT.
func isoStatusOfEnum(t *testing.T, pg *Postgres, status string) string {
	t.Helper()
	var got string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT iso_status($1::transfer_status)`, status).Scan(&got); err != nil {
		t.Fatalf("iso_status(%s): %v", status, err)
	}
	return got
}

// isoStatusOfTransfer reads iso_status(status) for a live transfers row.
func isoStatusOfTransfer(t *testing.T, pg *Postgres, id uuid.UUID) string {
	t.Helper()
	var got string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT iso_status(status) FROM transfers WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("iso_status of %s: %v", id, err)
	}
	return got
}

// TestIsoStatusMapping asserts the enum->ISO code table for every transfer_status.
func TestIsoStatusMapping(t *testing.T) {
	pg := newTestPG(t)
	want := map[string]string{
		"pending":      "PDNG",
		"held":         "PDNG",
		"under_review": "PDNG",
		"posted":       "ACSC",
		"failed":       "RJCT",
		"canceled":     "CANC",
		"reversed":     "ACSC",
	}
	for status, code := range want {
		if got := isoStatusOfEnum(t, pg, status); got != code {
			t.Errorf("iso_status(%s) = %s, want %s", status, got, code)
		}
	}
}

// TestIsoStatusLifecycle drives the states end-to-end and checks the projection.
func TestIsoStatusLifecycle(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// posted -> ACSC.
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 100_000)
	posted, err := testTransfer(ctx, pg, uuid.NewString(), a, b, 4_000, "x", sqlc.TransferKindTransfer)
	if err != nil || posted.Status != sqlc.TransferStatusPosted {
		t.Fatalf("seed posted: st=%s err=%v", posted.Status, err)
	}
	if got := isoStatusOfTransfer(t, pg, posted.TransferID); got != "ACSC" {
		t.Errorf("posted iso = %s, want ACSC", got)
	}

	// reversed original stays ACSC; the reversal transfer itself is ACSC.
	if _, err := pg.Queries.ReverseTransfer(ctx, sqlc.ReverseTransferParams{
		ID: posted.TransferID, IdempotencyKey: uuid.NewString(), Reason: "oops",
	}); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if st := transferStatus(t, pg, posted.TransferID); st != sqlc.TransferStatusReversed {
		t.Fatalf("original status = %s, want reversed", st)
	}
	if got := isoStatusOfTransfer(t, pg, posted.TransferID); got != "ACSC" {
		t.Errorf("reversed-original iso = %s, want ACSC (the original settled)", got)
	}
	var revID uuid.UUID
	if err := pg.Pool.QueryRow(ctx, `SELECT id FROM transfers WHERE reverses_id = $1`, posted.TransferID).Scan(&revID); err != nil {
		t.Fatalf("find reversal row: %v", err)
	}
	if got := isoStatusOfTransfer(t, pg, revID); got != "ACSC" {
		t.Errorf("reversal-transfer iso = %s, want ACSC", got)
	}

	// held -> PDNG (a 'review' rule parks a first payment; funds reserved, no ledger).
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	heldDst := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)
	addWarningRule(t, pg, "first_payment_to_payee", "", "review", false, 0, 100)
	heldTid, hs, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, heldDst, 5_000)
	if err != nil || hs != sqlc.TransferStatusHeld {
		t.Fatalf("stage held: st=%s err=%v", hs, err)
	}
	if got := isoStatusOfTransfer(t, pg, heldTid); got != "PDNG" {
		t.Errorf("held iso = %s, want PDNG", got)
	}

	// canceled -> CANC (a pending transfer withdrawn before posting).
	cancelSrc := mkAccount(t, pg, mkCustomer(t, pg))
	cancelDst := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, cancelSrc, 10_000)
	pend, err := testRequestTransfer(ctx, pg, uuid.NewString(), cancelSrc, cancelDst, 1_000, "p", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request pending (cancel): %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT cancel_transfer($1, $2)`, pend.TransferID, "test cancel"); err != nil {
		t.Fatalf("cancel_transfer: %v", err)
	}
	if got := isoStatusOfTransfer(t, pg, pend.TransferID); got != "CANC" {
		t.Errorf("canceled iso = %s, want CANC", got)
	}

	// failed -> RJCT (a pending transfer whose authorization hold lapses).
	failSrc := mkAccount(t, pg, mkCustomer(t, pg))
	failDst := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, failSrc, 10_000)
	expd, err := testRequestTransfer(ctx, pg, uuid.NewString(), failSrc, failDst, 1_000, "p", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request pending (expire): %v", err)
	}
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE holds SET expires_at = now() - interval '1 hour' WHERE transfer_id = $1 AND status = 'active'`,
		expd.TransferID); err != nil {
		t.Fatalf("backdate hold: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT expire_holds()`); err != nil {
		t.Fatalf("expire_holds: %v", err)
	}
	if st := transferStatus(t, pg, expd.TransferID); st != sqlc.TransferStatusFailed {
		t.Fatalf("expired status = %s, want failed", st)
	}
	if got := isoStatusOfTransfer(t, pg, expd.TransferID); got != "RJCT" {
		t.Errorf("failed iso = %s, want RJCT", got)
	}

	// Leave no parked funds behind for the shared-DB reconcile checks.
	if _, err := pg.Pool.Exec(ctx, `SELECT cancel_transfer($1, $2)`, heldTid, "cleanup"); err != nil {
		t.Fatalf("cleanup held: %v", err)
	}
	reconcileClean(t, pg)
}
