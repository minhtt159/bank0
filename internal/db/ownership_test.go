package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// TRANSFER-1: the client_* wrappers enforce debit-account ownership in the DB so the
// handlers drop their ownership-probe round trip. These pin the security semantics:
// create on a foreign debit -> 42501 (-> 403); post/cancel of a foreign transfer ->
// 'not found' (-> 404, hiding existence). sqlState() is defined in concurrency_test.go.

func TestClientTransferOwnership(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bob := mkCustomer(t, pg)
	bAcct := mkAccount(t, pg, bob)
	fund(t, pg, aAcct, 10_000)
	fund(t, pg, bAcct, 10_000)

	clientTransfer := func(subject, debit, credit uuid.UUID) error {
		var tid uuid.UUID
		var st sqlc.TransferStatus
		var replay bool
		return pg.Pool.QueryRow(ctx,
			`SELECT transfer_id, status, was_replay FROM client_transfer($1,$2,$3,$4,$5,$6)`,
			subject, uuid.NewString(), debit, credit, int64(1_000), "ownership test").Scan(&tid, &st, &replay)
	}

	// alice debiting her OWN account -> ok
	if err := clientTransfer(alice, aAcct, bAcct); err != nil {
		t.Fatalf("owned client_transfer: %v", err)
	}
	// alice debiting BOB's account -> 42501 (insufficient_privilege -> 403)
	if err := clientTransfer(alice, bAcct, aAcct); sqlState(err) != "42501" {
		t.Errorf("foreign-debit client_transfer sqlstate = %q, want 42501; err=%v", sqlState(err), err)
	}
	// debiting a NONEXISTENT account is also not-owned -> 42501 (no existence leak)
	if err := clientTransfer(alice, uuid.New(), bAcct); sqlState(err) != "42501" {
		t.Errorf("nonexistent-debit sqlstate = %q, want 42501", sqlState(err))
	}
	reconcileClean(t, pg)
}

func TestClientPostCancelOwnership(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bob := mkCustomer(t, pg)
	bAcct := mkAccount(t, pg, bob)
	fund(t, pg, aAcct, 10_000)

	// alice owns a pending transfer (debits her account).
	req, err := testRequestTransfer(ctx, pg, uuid.NewString(), aAcct, bAcct, 1_000, "pending", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	// bob posting alice's transfer -> 'not found' (P0001 -> 404), hiding existence.
	var st sqlc.TransferStatus
	err = pg.Pool.QueryRow(ctx, `SELECT client_post_transfer($1,$2)`, bob, req.TransferID).Scan(&st)
	if sqlState(err) != "P0001" || !strings.Contains(strings.ToLower(errMsg(err)), "not found") {
		t.Errorf("foreign post err = %v (sqlstate %q), want P0001 'not found'", err, sqlState(err))
	}
	// the transfer must still be pending — the failed foreign post changed nothing.
	if got, _ := pg.Queries.GetTransfer(ctx, req.TransferID); got.Status != sqlc.TransferStatusPending {
		t.Errorf("status after foreign post = %s, want pending", got.Status)
	}

	// bob canceling alice's transfer -> also 'not found'.
	err = pg.Pool.QueryRow(ctx, `SELECT client_cancel_transfer($1,$2,$3)`, bob, req.TransferID, "idor").Scan(&st)
	if sqlState(err) != "P0001" {
		t.Errorf("foreign cancel sqlstate = %q, want P0001", sqlState(err))
	}

	// alice cancels her OWN transfer -> ok.
	if err := pg.Pool.QueryRow(ctx, `SELECT client_cancel_transfer($1,$2,$3)`, alice, req.TransferID, "mine").Scan(&st); err != nil {
		t.Fatalf("owned cancel: %v", err)
	}
	if st != sqlc.TransferStatusCanceled {
		t.Errorf("owned cancel status = %s, want canceled", st)
	}
	reconcileClean(t, pg)
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
