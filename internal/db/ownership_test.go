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

// TestSetDefaultAccountOneDefaultInvariant: a user with two accounts can move the
// default from A to B, and there is always EXACTLY ONE default. set_default_account
// must clear the old flag in the same statement-pair as it sets the new one — the
// uq_accounts_one_default partial unique index makes a stale second default impossible
// (a failure to clear would raise 23505 inside the function).
func TestSetDefaultAccountOneDefaultInvariant(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)
	a := mkAccount(t, pg, owner) // first account is the default
	b := mkAccount(t, pg, owner)

	// Explicitly set A (already the default), then flip to B.
	if err := pg.Queries.SetDefaultAccount(ctx, sqlc.SetDefaultAccountParams{UserID: owner, AccountID: a}); err != nil {
		t.Fatalf("set default A: %v", err)
	}
	if err := pg.Queries.SetDefaultAccount(ctx, sqlc.SetDefaultAccountParams{UserID: owner, AccountID: b}); err != nil {
		t.Fatalf("set default B: %v", err)
	}

	var nDefault int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE user_id = $1 AND is_default`, owner).Scan(&nDefault); err != nil {
		t.Fatalf("count defaults: %v", err)
	}
	if nDefault != 1 {
		t.Fatalf("default account count = %d, want exactly 1", nDefault)
	}
	var defID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE user_id = $1 AND is_default`, owner).Scan(&defID); err != nil {
		t.Fatalf("read default: %v", err)
	}
	if defID != b {
		t.Errorf("default account = %s, want B (%s)", defID, b)
	}
}

// TestDeleteBeneficiaryOwnership: only the owner can delete their saved payee. A
// non-owner delete is refused (P0001 'not found', hiding existence) and leaves the
// row intact; the owner's delete removes it.
func TestDeleteBeneficiaryOwnership(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)
	stranger := mkCustomer(t, pg)
	// A payee account owned by a THIRD user (you can't save your own account).
	payee := mkAccount(t, pg, mkCustomer(t, pg))
	var payeeIban string
	if err := pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id = $1`, payee).Scan(&payeeIban); err != nil {
		t.Fatalf("read payee iban: %v", err)
	}

	var bid uuid.UUID
	if err := pg.Pool.QueryRow(ctx, `SELECT add_beneficiary($1,'pal',$2)`, owner, payeeIban).Scan(&bid); err != nil {
		t.Fatalf("add_beneficiary: %v", err)
	}

	// A non-owner cannot delete it: raises 'not found' (P0001) and the row survives.
	if _, err := pg.Pool.Exec(ctx, `SELECT delete_beneficiary($1,$2)`, stranger, bid); sqlState(err) != "P0001" {
		t.Errorf("foreign delete sqlstate = %q, want P0001; err=%v", sqlState(err), err)
	}
	if !rowExists(t, pg, `SELECT 1 FROM beneficiaries WHERE id = $1`, bid) {
		t.Fatal("non-owner delete removed the beneficiary")
	}

	// The owner deletes it successfully; the row is gone.
	if _, err := pg.Pool.Exec(ctx, `SELECT delete_beneficiary($1,$2)`, owner, bid); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if rowExists(t, pg, `SELECT 1 FROM beneficiaries WHERE id = $1`, bid) {
		t.Error("owner delete left the beneficiary behind")
	}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
