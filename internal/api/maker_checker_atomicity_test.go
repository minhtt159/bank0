package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// MAKER-CHECKER-ATOMICITY: an above-threshold console credit/withdrawal must stage
// the PENDING transfer + hold AND enqueue the 4-eyes approval row in ONE
// transaction. The old console handler did this in two autocommitted DB calls, so
// a failure between them could leave a held-funds transfer with no queue row.
// request_money_with_approval folds both into a single PL/pgSQL function: either
// BOTH the pending transfer (with hold) and the approval row commit, or NEITHER.

// TestMakerCheckerAtomicHappyPath: the single fn produces EXACTLY one pending
// transfer (hold reserves the funds) AND exactly one approval-queue row.
func TestMakerCheckerAtomicHappyPath(t *testing.T) {
	_, pg := newTestServer(t)
	ctx := context.Background()

	makerID, _ := mkUser(t, pg, sqlc.UserRoleAdmin)
	custID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	acct := mkAcct(t, pg, custID, 5_000_000) // €50k funded so a withdrawal can hold

	detail, _ := json.Marshal(map[string]any{"amount_minor": 2_000_000, "kind": "withdraw", "account_id": acct.String()})
	mc, err := pg.RequestMoneyWithApproval(ctx, makerID, uuid.NewString(), acct, 2_000_000,
		sqlc.TransferKindWithdrawal, "Console withdrawal (awaiting approval)", detail)
	if err != nil {
		t.Fatalf("RequestMoneyWithApproval: %v", err)
	}
	if mc.TransferID == uuid.Nil || mc.RequestID == uuid.Nil {
		t.Fatalf("expected non-nil transfer + request ids; got %+v", mc)
	}

	// The transfer exists and is PENDING (staged, not posted).
	tr, err := pg.Queries.GetTransfer(ctx, mc.TransferID)
	if err != nil {
		t.Fatalf("GetTransfer: %v", err)
	}
	if tr.Status != sqlc.TransferStatusPending {
		t.Errorf("staged transfer status = %s, want pending", tr.Status)
	}

	// The hold reserves the funds: ledger unchanged, available down by the amount.
	led, avail := acctBalance(t, pg, acct)
	if led != 5_000_000 || avail != 3_000_000 {
		t.Errorf("ledger=%d available=%d, want 5000000/3000000 (hold of 2000000)", led, avail)
	}

	// Exactly one pending approval-queue row targets this transfer.
	var nReq int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM admin_actions
		   WHERE action='approval_request' AND target_id=$1 AND approved_by IS NULL`,
		mc.TransferID).Scan(&nReq); err != nil {
		t.Fatalf("count approval rows: %v", err)
	}
	if nReq != 1 {
		t.Errorf("approval rows for transfer = %d, want exactly 1", nReq)
	}
}

// TestMakerCheckerAtomicRollback is the regression test for the orphaned-hold bug.
// We force the SECOND step (the admin_actions INSERT) to fail by passing a maker id
// that does not exist in users — the actor_user_id FK rejects it. Because the hold
// (step 1) and the approval insert (step 2) share one transaction, the failure must
// roll BOTH back: no hold survives, and no approval-queue row is created.
func TestMakerCheckerAtomicRollback(t *testing.T) {
	_, pg := newTestServer(t)
	ctx := context.Background()

	custID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	acct := mkAcct(t, pg, custID, 5_000_000)

	ledBefore, availBefore := acctBalance(t, pg, acct)

	// A maker id with no matching users row -> admin_actions.actor_user_id FK fails.
	ghostMaker := uuid.New()

	mc, err := pg.RequestMoneyWithApproval(ctx, ghostMaker, uuid.NewString(), acct, 2_000_000,
		sqlc.TransferKindWithdrawal, "Console withdrawal (awaiting approval)", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected the approval INSERT to fail on a missing maker; got id %s", mc.TransferID)
	}

	// NEITHER side effect may survive the failed transaction.
	led, avail := acctBalance(t, pg, acct)
	if led != ledBefore || avail != availBefore {
		t.Errorf("rolled-back staging must leave NO hold: ledger=%d available=%d, want %d/%d",
			led, avail, ledBefore, availBefore)
	}

	// No orphaned pending transfer for this account into external_clearing.
	var nPending int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM transfers
		   WHERE debit_account_id=$1 AND status='pending' AND kind='withdrawal'`,
		acct).Scan(&nPending); err != nil {
		t.Fatalf("count pending transfers: %v", err)
	}
	if nPending != 0 {
		t.Errorf("orphaned pending transfers = %d, want 0 (the hold must not survive)", nPending)
	}

	// And no approval-queue row created for this ghost maker.
	var nReq int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM admin_actions
		   WHERE action='approval_request' AND actor_user_id=$1`,
		ghostMaker).Scan(&nReq); err != nil {
		t.Fatalf("count approval rows: %v", err)
	}
	if nReq != 0 {
		t.Errorf("approval-queue rows = %d, want 0 (nothing should be enqueued)", nReq)
	}
}
