package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// These exercise the guard rails inside request_transfer (00008) and the account
// guards it leans on (00007). Each asserts the exact SQLSTATE the PL/pgSQL raises:
//   - guards with an explicit USING ERRCODE = 'check_violation' -> 23514
//   - bare RAISE EXCEPTION (no ERRCODE)                          -> P0001
//   - an invalid enum cast for p_kind                            -> 22P02
//
// We call the function directly over the raw pool (SELECT ... FROM request_transfer)
// rather than the Go wrappers, so the test pins the database contract itself.

// reqTransfer is a thin helper that invokes request_transfer with the default
// kind 'transfer' and returns the raised error (nil on success). It scans the
// RETURNS TABLE row so a successful call is fully exercised.
func reqTransfer(ctx context.Context, pg *Postgres, key string, debit, credit uuid.UUID, amount int64) error {
	var (
		tid    uuid.UUID
		status string
		replay bool
	)
	return pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay
		   FROM request_transfer($1, $2, $3, $4, $5, 'transfer')`,
		key, debit, credit, amount, "guard test",
	).Scan(&tid, &status, &replay)
}

// TestTransferLimitGuard: an amount above the debit account's transfer_limit_minor
// is rejected; a within-limit transfer of the same account succeeds. The limit
// check is a bare RAISE EXCEPTION (no ERRCODE) -> P0001.
func TestTransferLimitGuard(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)

	// Customer account with a deliberately small limit, funded well above it.
	ibanStr := genIban(t, pg)
	var debit uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT create_account($1, $2, $3, $4)`, owner, ibanStr, "1234", int64(5_000),
	).Scan(&debit); err != nil {
		t.Fatalf("create_account (limited): %v", err)
	}
	fund(t, pg, debit, 1_000_000)

	credit := mkAccount(t, pg, mkCustomer(t, pg))

	// Above the limit -> rejected.
	if err := reqTransfer(ctx, pg, uuid.NewString(), debit, credit, 5_001); err == nil {
		t.Fatal("transfer above limit must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Fatalf("limit breach SQLSTATE = %q, want P0001", got)
	}

	// Exactly at the limit -> allowed (limit check is `> limit`).
	if err := reqTransfer(ctx, pg, uuid.NewString(), debit, credit, 5_000); err != nil {
		t.Fatalf("within-limit transfer must succeed: %v", err)
	}
	reconcileClean(t, pg)
}

// TestFrozenDebitRejected: a frozen debit account fails request_transfer with the
// bare "not active" RAISE -> P0001. Money must not move.
func TestFrozenDebitRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	if _, err := pg.Pool.Exec(ctx, `SELECT set_account_status($1, 'frozen')`, debit); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	if err := reqTransfer(ctx, pg, uuid.NewString(), debit, credit, 1_000); err == nil {
		t.Fatal("debiting a frozen account must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Fatalf("frozen-debit SQLSTATE = %q, want P0001", got)
	}
	if led, _ := balance(t, pg, debit); led != 100_000 {
		t.Errorf("frozen-reject must not move money; ledger=%d want 100000", led)
	}
	reconcileClean(t, pg)
}

// TestClosedDebitRejected: a closed debit account is likewise rejected (P0001).
func TestClosedDebitRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	if _, err := pg.Pool.Exec(ctx, `SELECT set_account_status($1, 'closed')`, debit); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := reqTransfer(ctx, pg, uuid.NewString(), debit, credit, 1_000); err == nil {
		t.Fatal("debiting a closed account must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Fatalf("closed-debit SQLSTATE = %q, want P0001", got)
	}
	if led, _ := balance(t, pg, debit); led != 100_000 {
		t.Errorf("closed-reject must not move money; ledger=%d want 100000", led)
	}
	reconcileClean(t, pg)
}

// TestFrozenCreditRejected: the credit (destination) account being inactive is
// rejected too — both legs must be 'active'. P0001.
func TestFrozenCreditRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	if _, err := pg.Pool.Exec(ctx, `SELECT set_account_status($1, 'frozen')`, credit); err != nil {
		t.Fatalf("freeze credit: %v", err)
	}

	if err := reqTransfer(ctx, pg, uuid.NewString(), debit, credit, 1_000); err == nil {
		t.Fatal("crediting a frozen account must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Fatalf("frozen-credit SQLSTATE = %q, want P0001", got)
	}
	reconcileClean(t, pg)
}

// TestSingleCurrencyEnforcedAtSchema: the spec asks for a EUR<->USD currency
// mismatch, but the accounts table carries an unconditional CHECK (currency =
// 'EUR') (db/migrations/00003_init_tables.sql), so a USD account can never be
// inserted in the first place. That means request_transfer's own "currency
// mismatch" branch is currently UNREACHABLE: the schema forbids the precondition.
// We assert the schema constraint instead — a USD account insert is rejected with
// check_violation (23514) — which documents why the mismatch branch can't fire.
func TestSingleCurrencyEnforcedAtSchema(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)
	ibanStr := genIban(t, pg)

	_, err := pg.Pool.Exec(ctx,
		`INSERT INTO accounts (user_id, kind, iban, pin_hash, currency, status)
		 VALUES ($1, 'customer', $2, crypt('1234', gen_salt('bf', 4)), 'USD', 'active')`,
		owner, ibanStr,
	)
	if err == nil {
		t.Fatal("inserting a non-EUR account must be rejected by the currency CHECK")
	}
	if got := sqlstate(err); got != "23514" {
		t.Fatalf("USD account insert SQLSTATE = %q, want 23514 (check_violation)", got)
	}
}

// TestNonPositiveAmountRejected: amount 0 and amount -100 both fail (P0001).
func TestNonPositiveAmountRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	for _, amt := range []int64{0, -100} {
		if err := reqTransfer(ctx, pg, uuid.NewString(), debit, credit, amt); err == nil {
			t.Fatalf("amount %d must be rejected", amt)
		} else if got := sqlstate(err); got != "P0001" {
			t.Fatalf("amount=%d SQLSTATE = %q, want P0001", amt, got)
		}
	}
	reconcileClean(t, pg)
}

// TestSameAccountRejected: debit == credit is rejected (P0001).
func TestSameAccountRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	acct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, acct, 100_000)

	if err := reqTransfer(ctx, pg, uuid.NewString(), acct, acct, 1_000); err == nil {
		t.Fatal("debit == credit must be rejected")
	} else if got := sqlstate(err); got != "P0001" {
		t.Fatalf("same-account SQLSTATE = %q, want P0001", got)
	}
	reconcileClean(t, pg)
}

// TestIdempotencyConflictRejected: the same key reused with DIFFERENT params is a
// check_violation (23514). An identical replay must NOT fail and reports
// was_replay = true.
func TestIdempotencyConflictRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	key := uuid.NewString()
	if err := reqTransfer(ctx, pg, key, debit, credit, 1_000); err != nil {
		t.Fatalf("first request_transfer: %v", err)
	}

	// Same key, different amount -> conflict.
	if err := reqTransfer(ctx, pg, key, debit, credit, 2_000); err == nil {
		t.Fatal("key reuse with different params must be rejected")
	} else if got := sqlstate(err); got != "23514" {
		t.Fatalf("idempotency conflict SQLSTATE = %q, want 23514 (check_violation)", got)
	}

	// Same key, identical params -> replay, no error, was_replay = true.
	var (
		tid    uuid.UUID
		status string
		replay bool
	)
	if err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay
		   FROM request_transfer($1, $2, $3, $4, $5, 'transfer')`,
		key, debit, credit, int64(1_000), "guard test",
	).Scan(&tid, &status, &replay); err != nil {
		t.Fatalf("identical replay must succeed: %v", err)
	}
	if !replay {
		t.Errorf("identical replay should report was_replay = true; got false")
	}
}

// TestInvalidKindRejected: an unknown transfer_kind value fails the enum cast
// (invalid_text_representation -> 22P02) before any business logic runs.
func TestInvalidKindRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	var tid uuid.UUID
	err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id FROM request_transfer($1, $2, $3, $4, $5, 'bogus')`,
		uuid.NewString(), debit, credit, int64(1_000), "guard test",
	).Scan(&tid)
	if err == nil {
		t.Fatal("p_kind='bogus' must be rejected by the enum cast")
	}
	if got := sqlstate(err); got != "22P02" {
		t.Fatalf("invalid kind SQLSTATE = %q, want 22P02 (invalid_text_representation)", got)
	}
}

// TestEmptyIdempotencyKeyRejected: an empty key fails the explicit guard with
// check_violation (23514).
func TestEmptyIdempotencyKeyRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	debit := mkAccount(t, pg, mkCustomer(t, pg))
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, debit, 100_000)

	if err := reqTransfer(ctx, pg, "", debit, credit, 1_000); err == nil {
		t.Fatal("empty idempotency key must be rejected")
	} else if got := sqlstate(err); got != "23514" {
		t.Fatalf("empty-key SQLSTATE = %q, want 23514 (check_violation)", got)
	}
}

// genIban builds a checksum-valid SE IBAN via the DB's own generator so the
// accounts.iban CHECK (iban_is_valid) accepts it — mirroring mkAccount's pattern
// without going through create_account.
func genIban(t *testing.T, pg *Postgres) string {
	t.Helper()
	var ibanStr string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT iban_generate('SE', $1)`, uniqHex(20),
	).Scan(&ibanStr); err != nil {
		t.Fatalf("iban_generate: %v", err)
	}
	return ibanStr
}

// clientTransferRes drives client_transfer — which namespaces the idempotency key
// to the authenticated subject (Rec 3) — and returns the transfer id + replay flag.
func clientTransferRes(ctx context.Context, pg *Postgres, subject uuid.UUID, key string, debit, credit uuid.UUID, amount int64) (uuid.UUID, bool, error) {
	var tid uuid.UUID
	var st string
	var replay bool
	err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay FROM client_transfer($1,$2,$3,$4,$5,$6)`,
		subject, key, debit, credit, amount, "per-owner idem").Scan(&tid, &st, &replay)
	return tid, replay, err
}

// TestIdempotencyKeyPerOwner: the idempotency namespace is bound to the owning
// principal, so the SAME raw key used by two DIFFERENT customers yields two
// independent claims — neither surfaces or collides with the other's result.
func TestIdempotencyKeyPerOwner(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bob := mkCustomer(t, pg)
	bAcct := mkAccount(t, pg, bob)
	sink := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)
	fund(t, pg, bAcct, 100_000)

	key := uuid.NewString() // the SAME raw idempotency key for both customers

	aliceTid, aliceReplay, err := clientTransferRes(ctx, pg, alice, key, aAcct, sink, 1_000)
	if err != nil {
		t.Fatalf("alice transfer: %v", err)
	}
	if aliceReplay {
		t.Error("alice's first use of the key must not be a replay")
	}

	// bob reuses the identical key in his OWN namespace: an independent claim that
	// must succeed and must NOT surface alice's stored result.
	bobTid, bobReplay, err := clientTransferRes(ctx, pg, bob, key, bAcct, sink, 2_000)
	if err != nil {
		t.Fatalf("bob transfer with same key: %v", err)
	}
	if bobReplay {
		t.Error("bob's use of the same key in his own namespace must not be a replay")
	}
	if aliceTid == bobTid {
		t.Fatalf("same key across owners must yield independent transfers; both = %s", aliceTid)
	}
	reconcileClean(t, pg)
}

// TestIdempotencyReplayWithinOwner: WITHIN one owner's namespace the replay and
// fingerprint semantics are unchanged — an identical (owner,key,params) call is a
// replay returning the original transfer; the same (owner,key) with different
// params trips the fingerprint-mismatch check_violation (23514).
func TestIdempotencyReplayWithinOwner(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	sink := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)

	key := uuid.NewString()
	tid1, replay1, err := clientTransferRes(ctx, pg, alice, key, aAcct, sink, 3_000)
	if err != nil || replay1 {
		t.Fatalf("first: err=%v replay=%v", err, replay1)
	}

	// identical (owner,key,params) -> replay, same transfer id, single debit.
	tid2, replay2, err := clientTransferRes(ctx, pg, alice, key, aAcct, sink, 3_000)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay2 || tid2 != tid1 {
		t.Errorf("same (owner,key) replay should return original; tid1=%s tid2=%s replay=%v", tid1, tid2, replay2)
	}
	if lb, _ := balance(t, pg, aAcct); lb != 97_000 {
		t.Errorf("replay must not double-debit; balance=%d want 97000", lb)
	}

	// same (owner,key), DIFFERENT amount -> fingerprint mismatch (23514).
	if _, _, err := clientTransferRes(ctx, pg, alice, key, aAcct, sink, 5_000); err == nil {
		t.Fatal("same (owner,key) with different amount must be rejected")
	} else if got := sqlstate(err); got != "23514" {
		t.Fatalf("fingerprint-mismatch SQLSTATE = %q, want 23514 (check_violation)", got)
	}
	reconcileClean(t, pg)
}
