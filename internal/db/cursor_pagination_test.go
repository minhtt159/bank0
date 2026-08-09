package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// CURSOR-ID-OPTIONAL: cursor_id is optional in the OpenAPI contract, so a client
// may page with the timestamp alone. Comparing (ts, id) < (cursor, NULL) makes
// every row tying the cursor timestamp evaluate NULL — they silently vanish at the
// page boundary, the exact bug the composite cursor was added to fix. COALESCE to
// the max UUID keeps a cursor-only page complete.
func TestListMyTransfersCursorWithoutCursorID(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)
	a := mkAccount(t, pg, owner)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	for i := 0; i < 3; i++ {
		if _, err := testTransfer(ctx, pg, uuid.NewString(), a, b, 100, "tie", sqlc.TransferKindTransfer); err != nil {
			t.Fatalf("transfer %d: %v", i, err)
		}
	}
	// Force an exact timestamp tie across the page boundary — the case a
	// timestamp-only cursor cannot disambiguate.
	tie := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE transfers SET requested_at = $1 WHERE kind = 'transfer'`, tie); err != nil {
		t.Fatalf("force tie: %v", err)
	}

	rows, err := pg.Queries.ListMyTransfers(ctx, sqlc.ListMyTransfersParams{
		Subject:   owner,
		Cursor:    &tie, // cursor only — no cursor_id, as the contract allows
		PageLimit: 10,
	})
	if err != nil {
		t.Fatalf("ListMyTransfers: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("rows on a cursor-only page = %d, want 3 (rows tying the cursor timestamp were dropped)", len(rows))
	}
}
