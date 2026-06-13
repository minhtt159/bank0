package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// TestLedgerKeysetCoversTies is the regression test for the client ledger cursor
// bug: many debits posted in ONE transaction share an identical posted_at, so the
// old posted_at-only cursor silently skipped them at a page boundary. The composite
// (posted_at, id) keyset must page through ALL of them with no skips or duplicates.
func TestLedgerKeysetCoversTies(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 1_000_000)

	const n = 25
	tx, err := pg.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		var tid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT transfer_id FROM transfer($1,$2,$3,$4,$5,'transfer')`,
			uuid.NewString(), a, b, int64(100), fmt.Sprintf("tie #%d", i)).Scan(&tid); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("seed transfer %d: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil { // all n debits on `a` now share one posted_at
		t.Fatal(err)
	}

	seen := map[uuid.UUID]bool{}
	debits := 0
	var curTS *time.Time
	var curID *uuid.UUID
	for page := 0; page < 100; page++ {
		rows, err := pg.Queries.GetAccountLedger(ctx, sqlc.GetAccountLedgerParams{
			AccountID: a, Cursor: curTS, CursorID: curID, PageLimit: 7,
		})
		if err != nil {
			t.Fatalf("ledger page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("duplicate ledger row across pages: %s", r.ID)
			}
			seen[r.ID] = true
			if string(r.Direction) == "debit" {
				debits++
			}
		}
		last := rows[len(rows)-1]
		curTS, curID = &last.PostedAt, &last.ID
		if len(rows) < 7 {
			break
		}
	}
	if debits != n {
		t.Fatalf("keyset pagination lost tied rows: got %d debit entries, want %d", debits, n)
	}
}
