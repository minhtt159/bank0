package db

import (
	"context"
	"testing"

	"github.com/minhtt159/bank0/internal/migrate"
)

// TestMigrationsReversible proves every goose migration ships a working
// `-- +goose Down`: against an ISOLATED throwaway database it runs
// Up -> Down all the way to zero (one migration at a time) -> Up again, asserting
// no error at any step. A migration whose Down is missing or broken fails here
// rather than during a production rollback. It never touches the shared bank0_test
// the rest of the suite uses, and skips without TEST_DATABASE_DSN.
func TestMigrationsReversible(t *testing.T) {
	if testDSN == "" {
		t.Skip("set TEST_DATABASE_DSN to run DB integration tests")
	}
	ctx := context.Background()

	// Isolated database. CREATE/DROP DATABASE can't run on a connection to the
	// target, so issue them over the base (testDSN) connection. FORCE drops any
	// connection lingering from a previous run. Mirrors seed_demo_test.go.
	const migDB = "bank0_migrate_test"
	base := newRawPG(t, testDSN)
	if _, err := base.Pool.Exec(ctx, `DROP DATABASE IF EXISTS `+migDB+` WITH (FORCE)`); err != nil {
		t.Fatalf("drop migrate db: %v", err)
	}
	if _, err := base.Pool.Exec(ctx, `CREATE DATABASE `+migDB); err != nil {
		t.Fatalf("create migrate db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = base.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+migDB+` WITH (FORCE)`)
		base.Close()
	})

	migDSN := swapDBName(testDSN, migDB)

	// Up: apply the whole stack to the fresh DB.
	if err := migrate.Up(migDSN); err != nil {
		t.Fatalf("initial migrate up: %v", err)
	}

	// Down to zero, one migration per step. goose.Down rolls back a single version;
	// we drive it off goose_db_version so we stop exactly at the base (version 0) and
	// assert every Down leg succeeds — a broken `-- +goose Down` trips here.
	pg := newRawPG(t, migDSN)
	t.Cleanup(pg.Close) // LIFO: runs before the DROP above
	for step := 0; step < 100; step++ {
		var v int64
		if err := pg.Pool.QueryRow(ctx,
			`SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
			t.Fatalf("read goose version: %v", err)
		}
		if v == 0 {
			break
		}
		if err := migrate.Down(migDSN); err != nil {
			t.Fatalf("migrate down from version %d: %v", v, err)
		}
	}

	// A full down really removed the domain schema (the ledger table is gone).
	var ledgerExists bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.ledger_entries') IS NOT NULL`).Scan(&ledgerExists); err != nil {
		t.Fatalf("probe schema after down: %v", err)
	}
	if ledgerExists {
		t.Error("ledger_entries still exists after migrating fully down: a Down leg is incomplete")
	}

	// Up again: the whole stack must re-apply cleanly on the emptied DB.
	if err := migrate.Up(migDSN); err != nil {
		t.Fatalf("re-run migrate up: %v", err)
	}
}
