package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// accounts.held_minor is a trigger-maintained cache of SUM(active holds), like
// balance_minor caches the ledger. These pin that it tracks the hold lifecycle and
// that reconcile()'s held_drift invariant catches a divergence.

func heldMinor(t *testing.T, pg *Postgres, acct uuid.UUID) int64 {
	t.Helper()
	var h int64
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT held_minor FROM accounts WHERE id = $1`, acct).Scan(&h); err != nil {
		t.Fatalf("read held_minor: %v", err)
	}
	return h
}

func TestHeldCacheTracksLifecycle(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000) // deposit auto-posts: no lingering hold

	if h := heldMinor(t, pg, a); h != 0 {
		t.Fatalf("initial held_minor = %d, want 0", h)
	}

	// request -> active hold -> held rises; available drops; ledger unchanged.
	req, err := testRequestTransfer(ctx, pg, uuid.NewString(), a, b, 4_000, "pending", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if h := heldMinor(t, pg, a); h != 4_000 {
		t.Errorf("held after request = %d, want 4000", h)
	}
	if led, avail := balance(t, pg, a); led != 10_000 || avail != 6_000 {
		t.Errorf("ledger=%d available=%d, want 10000/6000 (available via the cache)", led, avail)
	}

	// post -> hold captured -> held back to 0.
	if _, err := pg.Queries.PostTransfer(ctx, req.TransferID); err != nil {
		t.Fatalf("post: %v", err)
	}
	if h := heldMinor(t, pg, a); h != 0 {
		t.Errorf("held after post = %d, want 0", h)
	}

	// request again, then cancel -> hold released -> held back to 0.
	req2, _ := testRequestTransfer(ctx, pg, uuid.NewString(), a, b, 1_500, "pending2", sqlc.TransferKindTransfer)
	if h := heldMinor(t, pg, a); h != 1_500 {
		t.Errorf("held after request2 = %d, want 1500", h)
	}
	if _, err := pg.Queries.CancelTransfer(ctx, sqlc.CancelTransferParams{ID: req2.TransferID, Reason: "x"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if h := heldMinor(t, pg, a); h != 0 {
		t.Errorf("held after cancel = %d, want 0", h)
	}
	reconcileClean(t, pg)
}

func TestReconcileCatchesHeldDrift(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))

	// Corrupt the cache directly (a raw UPDATE doesn't fire the holds trigger), then
	// restore so the global reconcile stays clean for other tests.
	if _, err := pg.Pool.Exec(ctx, `UPDATE accounts SET held_minor = held_minor + 999 WHERE id = $1`, a); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `UPDATE accounts SET held_minor = held_minor - 999 WHERE id = $1`, a)
	})

	issues, err := pg.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	found := false
	for _, iss := range issues {
		if iss.CheckName == "held_drift" && strings.Contains(iss.Detail, a.String()) {
			found = true
		}
	}
	if !found {
		t.Errorf("reconcile did not report held_drift for the corrupted account; issues=%+v", issues)
	}
}
