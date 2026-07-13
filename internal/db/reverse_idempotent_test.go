package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Rec 4: a second reverse of an already-reversed transfer is IDEMPOTENT, not an
// error. Every reverse — under any key — converges on the one existing reversal,
// and the newly-claimed key is completed pointing at it.
func TestReverseTwiceIsIdempotent(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	res, err := testTransfer(ctx, pg, uuid.NewString(), a, b, 4_000, "to reverse", sqlc.TransferKindTransfer)
	if err != nil || res.Status != sqlc.TransferStatusPosted {
		t.Fatalf("transfer: st=%s err=%v", res.Status, err)
	}

	reverse := func(key string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pg.Pool.QueryRow(ctx, `SELECT reverse_transfer($1,$2,$3)`, res.TransferID, key, "clawback").Scan(&id); err != nil {
			t.Fatalf("reverse key=%s: %v", key, err)
		}
		return id
	}

	key1, key2 := uuid.NewString(), uuid.NewString()
	rev1 := reverse(key1)                 // first reversal
	rev2 := reverse(key2)                 // second reverse, DIFFERENT key
	if rev2 != rev1 {
		t.Fatalf("second reverse returned %s, want the existing reversal %s", rev2, rev1)
	}

	// The newly-claimed key2 is completed and points at the existing reversal.
	var status string
	var storedID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status, transfer_id FROM idempotency_keys
		  WHERE owner_id = '00000000-0000-0000-0000-000000000000' AND key = $1`, key2).Scan(&status, &storedID); err != nil {
		t.Fatalf("read key2: %v", err)
	}
	if status != "completed" || storedID != rev1 {
		t.Errorf("key2 = (%s, %s), want (completed, %s)", status, storedID, rev1)
	}

	// Replays under either key stay stable on the same reversal id.
	if got := reverse(key1); got != rev1 {
		t.Errorf("replay key1 = %s, want %s", got, rev1)
	}
	if got := reverse(key2); got != rev1 {
		t.Errorf("replay key2 = %s, want %s", got, rev1)
	}

	// Exactly ONE reversal exists for the original; the original is 'reversed'.
	var nRev int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE reverses_id = $1`, res.TransferID).Scan(&nRev); err != nil {
		t.Fatalf("count reversals: %v", err)
	}
	if nRev != 1 {
		t.Errorf("reversal count = %d, want 1", nRev)
	}
	if transferStatus(t, pg, res.TransferID) != sqlc.TransferStatusReversed {
		t.Errorf("original status = %s, want reversed", transferStatus(t, pg, res.TransferID))
	}
	reconcileClean(t, pg)
}
