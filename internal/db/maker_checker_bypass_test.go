package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/db/sqlc"
)

// MAKER-CHECKER-BYPASS-CLIENT: request_money_with_approval stages a withdrawal by
// DEBITING the customer's own account, so an ownership-only check would let the
// customer post it themselves before any approver acts — 4-eyes defeated from the
// client API. client_post_transfer must refuse while the approval row is open.
func TestClientCannotPostTransferAwaitingApproval(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	customer := mkCustomer(t, pg)
	maker := mkCustomer(t, pg)
	acct := mkAccount(t, pg, customer)
	fund(t, pg, acct, 5_000_000)

	mc, err := pg.RequestMoneyWithApproval(ctx, maker, uuid.NewString(), acct, 2_000_000,
		sqlc.TransferKindWithdrawal, "big cash out", nil)
	if err != nil {
		t.Fatalf("request_money_with_approval: %v", err)
	}

	// The customer owns the debit account — ownership passes, policy must not.
	_, err = pg.Pool.Exec(ctx, `SELECT client_post_transfer($1, $2)`, customer, mc.TransferID)
	if err == nil {
		t.Fatal("customer posted a transfer awaiting approval — 4-eyes bypassed")
	}
	if got := sqlstate(err); got != "P0001" {
		t.Errorf("sqlstate = %q, want P0001 (business raise -> 409)", got)
	}
	if got, _ := pg.Queries.GetTransfer(ctx, mc.TransferID); got.Status != sqlc.TransferStatusPending {
		t.Errorf("status = %s, want pending (the refused post must not move it)", got.Status)
	}

	// Once a second admin approves, the money moves through approve_request.
	checker := mkCustomer(t, pg)
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, mc.RequestID, checker); err != nil {
		t.Fatalf("approve_request: %v", err)
	}
	if got, _ := pg.Queries.GetTransfer(ctx, mc.TransferID); got.Status != sqlc.TransferStatusPosted {
		t.Errorf("status after approval = %s, want posted", got.Status)
	}
	reconcileClean(t, pg)
}

// MAKER-CHECKER-BYPASS-ADMIN: the admin JSON surface posted any amount directly —
// threshold routing lived only in the console's Go code. The decision belongs in
// the DB (rule 1), so admin_deposit/admin_withdraw refuse above-threshold posts
// for every caller; the console's read-then-post TOCTOU dies with it.
func TestAdminMoneyMoveRefusedAboveThreshold(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	acct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, acct, 5_000_000)

	// Default threshold is 1_000_000 minor and the test is `>`, so 1_000_000 posts.
	if _, err := pg.Queries.Deposit(ctx, sqlc.DepositParams{
		IdempotencyKey: uuid.NewString(), AccountID: acct, AmountMinor: 1_000_000, Description: "at threshold",
	}); err != nil {
		t.Fatalf("at-threshold deposit must post: %v", err)
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"deposit", func() error {
			_, err := pg.Queries.Deposit(ctx, sqlc.DepositParams{
				IdempotencyKey: uuid.NewString(), AccountID: acct, AmountMinor: 1_000_001, Description: "over",
			})
			return err
		}},
		{"withdraw", func() error {
			_, err := pg.Queries.Withdraw(ctx, sqlc.WithdrawParams{
				IdempotencyKey: uuid.NewString(), AccountID: acct, AmountMinor: 1_000_001, Description: "over",
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("above-threshold move posted without approval")
			}
			if got := sqlstate(err); got != "P0001" {
				t.Errorf("sqlstate = %q, want P0001", got)
			}
		})
	}
	reconcileClean(t, pg)
}
