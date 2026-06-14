package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Integrity triggers from migration 00005_triggers.sql.
//
// Both tamper guards (account_guard_balance, ledger_block_mutation) RAISE with
// USING ERRCODE = 'restrict_violation' -> SQLSTATE 23001. set_updated_at stamps
// NEW.updated_at := now() on every UPDATE of users/accounts/transfers.

const errRestrictViolation = "23001"

// account_guard_balance: balance_minor may change ONLY via the ledger trigger.
// A direct UPDATE on accounts.balance_minor must be rejected, and the cached
// balance must be left untouched.
func TestAccountGuardBalanceRejectsDirectUpdate(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	acct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, acct, 5_000)

	before, _ := balance(t, pg, acct)
	if before != 5_000 {
		t.Fatalf("setup: ledger balance = %d, want 5000", before)
	}

	// Direct tamper: bump the cached balance outside the ledger path.
	_, err := pg.Pool.Exec(ctx,
		`UPDATE accounts SET balance_minor = balance_minor + 1000000 WHERE id = $1`, acct)
	if err == nil {
		t.Fatal("direct UPDATE of accounts.balance_minor must be rejected by account_guard_balance")
	}
	if got := sqlstate(err); got != errRestrictViolation {
		t.Errorf("guard SQLSTATE = %q, want %q (restrict_violation)", got, errRestrictViolation)
	}

	// The cache must be unchanged by the rejected (rolled-back) statement.
	after, _ := balance(t, pg, acct)
	if after != before {
		t.Errorf("balance changed despite rejected tamper: before=%d after=%d", before, after)
	}
}

// ledger_block_mutation: ledger_entries is append-only. Both UPDATE and DELETE
// on an existing entry must be rejected with restrict_violation.
func TestLedgerEntriesAppendOnly(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	// A posted transfer writes ledger_entries (one debit on a, one credit on b).
	var transferID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id FROM transfer($1,$2,$3,$4,$5,'transfer')`,
		uuid.NewString(), a, b, int64(2_000), "append-only test").Scan(&transferID); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var entryID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT id FROM ledger_entries WHERE account_id = $1 LIMIT 1`, a).Scan(&entryID); err != nil {
		t.Fatalf("select ledger entry: %v", err)
	}

	// UPDATE must be blocked.
	_, err := pg.Pool.Exec(ctx,
		`UPDATE ledger_entries SET amount_minor = amount_minor + 1 WHERE id = $1`, entryID)
	if err == nil {
		t.Fatal("UPDATE on ledger_entries must be rejected (append-only)")
	}
	if got := sqlstate(err); got != errRestrictViolation {
		t.Errorf("UPDATE block SQLSTATE = %q, want %q (restrict_violation)", got, errRestrictViolation)
	}

	// DELETE must be blocked.
	_, err = pg.Pool.Exec(ctx, `DELETE FROM ledger_entries WHERE id = $1`, entryID)
	if err == nil {
		t.Fatal("DELETE on ledger_entries must be rejected (append-only)")
	}
	if got := sqlstate(err); got != errRestrictViolation {
		t.Errorf("DELETE block SQLSTATE = %q, want %q (restrict_violation)", got, errRestrictViolation)
	}

	// The entry must still exist and be unchanged.
	var stillThere bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE id = $1)`, entryID).Scan(&stillThere); err != nil {
		t.Fatalf("existence check: %v", err)
	}
	if !stillThere {
		t.Error("ledger entry vanished despite append-only guard")
	}
}

// set_updated_at: an allowed UPDATE on users (no balance/append guard there)
// must advance updated_at to now().
func TestSetUpdatedAtAdvancesOnUsersUpdate(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	uid := mkCustomer(t, pg)

	var before time.Time
	if err := pg.Pool.QueryRow(ctx,
		`SELECT updated_at FROM users WHERE id = $1`, uid).Scan(&before); err != nil {
		t.Fatalf("read initial updated_at: %v", err)
	}
	if before.IsZero() {
		t.Fatal("initial updated_at is zero/null")
	}

	// Nudge the clock forward inside Postgres so the comparison is robust even at
	// second resolution; keeps wall-clock sleep out of the test.
	if _, err := pg.Pool.Exec(ctx, `SELECT pg_sleep(0.01)`); err != nil {
		t.Fatalf("pg_sleep: %v", err)
	}

	if _, err := pg.Pool.Exec(ctx,
		`UPDATE users SET full_name = 'changed' WHERE id = $1`, uid); err != nil {
		t.Fatalf("update users: %v", err)
	}

	var after time.Time
	if err := pg.Pool.QueryRow(ctx,
		`SELECT updated_at FROM users WHERE id = $1`, uid).Scan(&after); err != nil {
		t.Fatalf("read new updated_at: %v", err)
	}
	if after.IsZero() {
		t.Fatal("updated_at became zero/null after update")
	}
	if after.Before(before) {
		t.Errorf("updated_at went backwards: before=%s after=%s", before, after)
	}
	if !after.After(before) {
		t.Errorf("updated_at did not advance: before=%s after=%s", before, after)
	}
}
