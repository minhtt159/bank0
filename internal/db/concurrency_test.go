package db

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// These exercise the money path under real concurrency — the gap the single-
// threaded suite can't cover. reconcile() is the oracle after every test: the
// ledger must still net to zero and balance_minor must equal SUM(ledger_entries).
// A load test that breaks an invariant is the most important signal a bank can get.

// sqlState returns the Postgres SQLSTATE of err, or "" if it isn't a PgError.
func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// TestConcurrentSameIdempotencyKey fires N concurrent transfers carrying the SAME
// Idempotency-Key. Exactly one may post; the rest must replay the original or hit
// in_progress (55006) — never double-debit. This is the ON CONFLICT DO NOTHING +
// in_progress state machine under the race it exists to defend against.
func TestConcurrentSameIdempotencyKey(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 1_000_000)

	const n = 16
	const amount = 1_000
	key := uuid.NewString()

	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[uuid.UUID]bool{}
	var posted, replayed, inProgress, other int

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := testTransfer(ctx, pg, key, a, b, amount, "same-key race", sqlc.TransferKindTransfer)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && res.WasReplay:
				replayed++
				ids[res.TransferID] = true
			case err == nil:
				posted++
				ids[res.TransferID] = true
			case sqlState(err) == "55006": // in_progress: a concurrent attempt holds the key
				inProgress++
			default:
				other++
				t.Errorf("unexpected error under same-key race: %v (sqlstate %q)", err, sqlState(err))
			}
		}()
	}
	wg.Wait()

	// Exactly one transfer id may exist for the key, and the money must move exactly
	// once regardless of how the N attempts split across post/replay/in_progress.
	if len(ids) > 1 {
		t.Errorf("same key produced %d distinct transfer ids, want at most 1: %v", len(ids), ids)
	}
	if other != 0 {
		t.Fatalf("%d attempts failed with an unexpected error", other)
	}
	if lb, _ := balance(t, pg, a); lb != 1_000_000-amount {
		t.Errorf("debit balance = %d, want %d (exactly ONE debit under %d concurrent same-key attempts; posted=%d replayed=%d in_progress=%d)",
			lb, 1_000_000-amount, n, posted, replayed, inProgress)
	}
	if lb, _ := balance(t, pg, b); lb != amount {
		t.Errorf("credit balance = %d, want %d (exactly one credit)", lb, amount)
	}
	reconcileClean(t, pg)
}

// TestConcurrentTransfersSharedDebit drives N concurrent transfers (distinct keys)
// out of ONE shared account — the contention shape of the EXTERNAL_CLEARING hot row
// and any popular account. The balance-cache trigger serializes per-account writes;
// this proves they serialize correctly (no lost updates) rather than racing.
func TestConcurrentTransfersSharedDebit(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	src := mkAccount(t, pg, mkCustomer(t, pg))

	const n = 20
	const amount = 1_000
	fund(t, pg, src, n*amount) // exactly enough for all N

	dests := make([]uuid.UUID, n)
	for i := range dests {
		dests[i] = mkAccount(t, pg, mkCustomer(t, pg))
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = testTransfer(ctx, pg, uuid.NewString(), src, dests[i], amount, "fan-out", sqlc.TransferKindTransfer)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("transfer %d failed: %v (sqlstate %q)", i, err, sqlState(err))
		}
	}
	// Every debit landed exactly once: the shared account is drained to zero, and each
	// destination got exactly one credit — no lost updates from the concurrent writes.
	if lb, _ := balance(t, pg, src); lb != 0 {
		t.Errorf("shared debit balance = %d, want 0 (all %d debits applied, none lost)", lb, n)
	}
	for i, d := range dests {
		if lb, _ := balance(t, pg, d); lb != amount {
			t.Errorf("dest %d balance = %d, want %d", i, lb, amount)
		}
	}
	reconcileClean(t, pg)
}

// TestConcurrentDeadlockOrdering hammers a pair of accounts with transfers in BOTH
// directions at once. request_transfer locks the two account rows in deterministic
// lowest-id-first order; if that ordering ever regressed, Postgres would detect a
// deadlock (40P01) and abort a transaction. The guarantee is asserted here, not just
// by reading the code: no deadlock, money conserved, invariants intact.
func TestConcurrentDeadlockOrdering(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	const start = 1_000_000
	fund(t, pg, a, start)
	fund(t, pg, b, start)

	const pairs = 24
	const amount = 100

	var wg sync.WaitGroup
	errs := make([]error, 2*pairs)
	for i := 0; i < pairs; i++ {
		wg.Add(2)
		go func(i int) { // A -> B
			defer wg.Done()
			_, errs[2*i] = testTransfer(ctx, pg, uuid.NewString(), a, b, amount, "a->b", sqlc.TransferKindTransfer)
		}(i)
		go func(i int) { // B -> A, the opposite lock order if it weren't normalized
			defer wg.Done()
			_, errs[2*i+1] = testTransfer(ctx, pg, uuid.NewString(), b, a, amount, "b->a", sqlc.TransferKindTransfer)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			if sqlState(err) == "40P01" {
				t.Fatalf("DEADLOCK (40P01) on transfer %d — lowest-id-first lock ordering regressed: %v", i, err)
			}
			t.Errorf("transfer %d failed: %v (sqlstate %q)", i, err, sqlState(err))
		}
	}
	// Equal traffic both ways nets to zero movement; total money is always conserved.
	la, _ := balance(t, pg, a)
	lb, _ := balance(t, pg, b)
	if la+lb != 2*start {
		t.Errorf("money not conserved: a=%d b=%d sum=%d, want %d", la, lb, la+lb, 2*start)
	}
	if la != start || lb != start {
		t.Errorf("balanced two-way traffic should net to zero: a=%d b=%d, want %d/%d", la, lb, start, start)
	}
	reconcileClean(t, pg)
}
