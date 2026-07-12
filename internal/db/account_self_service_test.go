package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Customer self-service account opening + limit-change maker-checker (00004/00006).

func openAccount(t *testing.T, pg *Postgres, key string, user uuid.UUID) (uuid.UUID, bool) {
	t.Helper()
	id, replay, err := pg.OpenCustomerAccount(context.Background(), key, user)
	if err != nil {
		t.Fatalf("open_customer_account: %v", err)
	}
	return id, replay
}

func TestOpenCustomerAccountMintsValidIban(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)

	a1, replay := openAccount(t, pg, uuid.NewString(), owner)
	if replay {
		t.Fatal("fresh open must not be a replay")
	}
	a2, _ := openAccount(t, pg, uuid.NewString(), owner)
	if a1 == a2 {
		t.Fatal("two opens must mint two accounts")
	}

	// Server-minted IBANs are REAL ISO 13616 SE IBANs: they pass the DB validator
	// (the accounts checksum CHECK already proved this at insert) and are distinct.
	var iban1, iban2 string
	var valid bool
	var limit int64
	var isDefault bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT iban, iban_is_valid(iban), transfer_limit_minor, is_default FROM accounts WHERE id = $1`, a1).
		Scan(&iban1, &valid, &limit, &isDefault); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if !valid || !strings.HasPrefix(iban1, "SE") || len(iban1) != 24 {
		t.Errorf("iban %q: valid=%v, want SE 24-char ISO IBAN", iban1, valid)
	}
	if !isDefault {
		t.Error("first account must be the default")
	}
	// Limit sourced from bank_settings.default_transfer_limit_minor at open time.
	var want int64
	if err := pg.Pool.QueryRow(ctx, `SELECT default_transfer_limit()`).Scan(&want); err != nil {
		t.Fatalf("default_transfer_limit: %v", err)
	}
	if limit != want {
		t.Errorf("limit = %d, want bank_settings default %d", limit, want)
	}
	if err := pg.Pool.QueryRow(ctx,
		`SELECT iban, is_default FROM accounts WHERE id = $1`, a2).Scan(&iban2, &isDefault); err != nil {
		t.Fatalf("read second account: %v", err)
	}
	if iban1 == iban2 {
		t.Error("sequence-minted IBANs must be distinct")
	}
	if isDefault {
		t.Error("second account must not be the default")
	}
}

func TestOpenCustomerAccountReplayAndCap(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)

	key := uuid.NewString()
	a1, _ := openAccount(t, pg, key, owner)
	a2, replay := openAccount(t, pg, key, owner)
	if !replay || a2 != a1 {
		t.Errorf("replay = (%s, %v), want (%s, true)", a2, replay, a1)
	}
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE user_id = $1`, owner).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("accounts after replay = %d, want 1", n)
	}

	// Per-owner namespace (Rec 3): the SAME key used by a DIFFERENT subject is an
	// independent claim in its own namespace — it opens that user's own account
	// rather than colliding with, or surfacing, owner's result.
	other := mkCustomer(t, pg)
	if oa, oreplay, err := pg.OpenCustomerAccount(ctx, key, other); err != nil || oreplay || oa == a1 {
		t.Errorf("cross-user same key = (%s, replay=%v, err=%v); want a fresh account, no replay, no error", oa, oreplay, err)
	}

	// Cap: bank_settings.max_accounts_per_user (seeded 5). Fill up, then overflow.
	var cap int
	if err := pg.Pool.QueryRow(ctx, `SELECT max_accounts_per_user()`).Scan(&cap); err != nil {
		t.Fatalf("max_accounts_per_user: %v", err)
	}
	for i := n; i < cap; i++ {
		openAccount(t, pg, uuid.NewString(), owner)
	}
	_, _, err := pg.OpenCustomerAccount(ctx, uuid.NewString(), owner)
	if sqlstate(err) != "23514" || !strings.Contains(err.Error(), "account limit") {
		t.Errorf("over-cap open = %v, want 23514 'account limit'", err)
	}

	// A closed account frees a slot.
	if _, err := pg.Pool.Exec(ctx, `SELECT set_account_status($1, 'closed')`, a1); err != nil {
		t.Fatalf("close: %v", err)
	}
	openAccount(t, pg, uuid.NewString(), owner) // must succeed again
}

func TestLimitChangeMakerChecker(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	customer := mkCustomer(t, pg)
	acct := mkAccount(t, pg, customer)
	admin := mkCustomer(t, pg) // any distinct user id stands in for the approver

	var before int64
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_limit_minor FROM accounts WHERE id = $1`, acct).Scan(&before); err != nil {
		t.Fatalf("read limit: %v", err)
	}

	// Requesting the unchanged limit is rejected.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT request_limit_change($1, $2, $3, 'same')`, acct, customer, before); sqlstate(err) != "23514" {
		t.Errorf("unchanged-limit SQLSTATE = %q, want 23514", sqlstate(err))
	}

	want := before + 100_000
	var reqID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT request_limit_change($1, $2, $3, 'travel')`, acct, customer, want).Scan(&reqID); err != nil {
		t.Fatalf("request_limit_change: %v", err)
	}

	// 4-eyes: the maker cannot apply their own request.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT approve_limit_change($1, $2)`, reqID, customer); sqlstate(err) != "42501" {
		t.Errorf("self-approve SQLSTATE = %q, want 42501", sqlstate(err))
	}

	// A different user applies it; the account limit changes.
	var target uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT approve_limit_change($1, $2)`, reqID, admin).Scan(&target); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var after int64
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_limit_minor FROM accounts WHERE id = $1`, acct).Scan(&after); err != nil {
		t.Fatalf("read limit: %v", err)
	}
	if target != acct || after != want {
		t.Errorf("after approve limit = %d (target %s), want %d (%s)", after, target, want, acct)
	}

	// Double-resolve -> 'already handled'.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT approve_limit_change($1, $2)`, reqID, admin); sqlstate(err) != "23514" {
		t.Errorf("double-approve SQLSTATE = %q, want 23514", sqlstate(err))
	}

	// Reject path: a second request, rejected, leaves the limit untouched.
	var req2 uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT request_limit_change($1, $2, $3, 'more')`, acct, customer, want+50_000).Scan(&req2); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT reject_limit_change($1, $2, 'no')`, req2, admin); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var final int64
	_ = pg.Pool.QueryRow(ctx, `SELECT transfer_limit_minor FROM accounts WHERE id = $1`, acct).Scan(&final)
	if final != want {
		t.Errorf("rejected request must not change the limit: %d, want %d", final, want)
	}
}

// TestUpdateTransferLimitGatesTransfers drives update_transfer_limit directly and
// proves it changes the per-transaction ceiling request_transfer enforces: below the
// amount the transfer is rejected and no money moves; once the limit is raised the
// same transfer posts. NOTE: the over-limit RAISE carries no custom ERRCODE, so it
// surfaces as P0001 'amount ... exceeds transfer limit ...' (mapDBError maps that to
// 422) — NOT the 23514 'account limit' code, which belongs to the account-count cap
// in open_customer_account.
func TestUpdateTransferLimitGatesTransfers(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 100_000)

	const amount = 40_000

	// Lower the debit account's limit below the amount -> the transfer is rejected.
	if err := pg.Queries.UpdateTransferLimit(ctx, sqlc.UpdateTransferLimitParams{
		AccountID: a, TransferLimitMinor: amount - 1,
	}); err != nil {
		t.Fatalf("lower limit: %v", err)
	}
	_, err := testTransfer(ctx, pg, uuid.NewString(), a, b, amount, "over limit", sqlc.TransferKindTransfer)
	if err == nil {
		t.Fatal("transfer above the limit must be rejected")
	}
	if got := sqlstate(err); got != "P0001" || !strings.Contains(strings.ToLower(err.Error()), "exceeds transfer limit") {
		t.Errorf("over-limit err = %v (sqlstate %q), want P0001 'exceeds transfer limit'", err, got)
	}
	if lb, _ := balance(t, pg, a); lb != 100_000 {
		t.Errorf("rejected transfer moved money: balance=%d, want 100000", lb)
	}

	// Raise the limit to exactly the amount; the same transfer now posts.
	if err := pg.Queries.UpdateTransferLimit(ctx, sqlc.UpdateTransferLimitParams{
		AccountID: a, TransferLimitMinor: amount,
	}); err != nil {
		t.Fatalf("raise limit: %v", err)
	}
	if _, err := testTransfer(ctx, pg, uuid.NewString(), a, b, amount, "at limit", sqlc.TransferKindTransfer); err != nil {
		t.Fatalf("transfer at the raised limit: %v", err)
	}
	if lb, _ := balance(t, pg, a); lb != 60_000 {
		t.Errorf("balance after transfer = %d, want 60000", lb)
	}
	reconcileClean(t, pg)
}

func TestTransferCarriesRailIdentifiers(t *testing.T) {
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
		   FROM transfer($1,$2,$3,$4,$5,'transfer',$6)`,
		uuid.NewString(), a, b, int64(1_000), "rail ids", "INV-2026-07/042").
		Scan(&tid, &status, &wasReplay); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var uetr uuid.UUID
	var e2e *string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT uetr, end_to_end_id FROM transfers WHERE id = $1`, tid).Scan(&uetr, &e2e); err != nil {
		t.Fatalf("read rail ids: %v", err)
	}
	if uetr == uuid.Nil {
		t.Error("uetr must be minted at insert")
	}
	if e2e == nil || *e2e != "INV-2026-07/042" {
		t.Errorf("end_to_end_id = %v, want INV-2026-07/042", e2e)
	}

	// The ISO charset CHECK rejects garbage.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT transfer($1,$2,$3,$4,$5,'transfer',$6)`,
		uuid.NewString(), a, b, int64(1_000), "bad e2e", "nope\x01"); sqlstate(err) != "23514" {
		t.Errorf("bad end_to_end_id SQLSTATE = %q, want 23514", sqlstate(err))
	}
}
