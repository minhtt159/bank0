package db

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/migrate"
)

// TestDemoSeedLoadsCleanAndReconciles generates a demo seed with db/seedgen, loads
// it into an ISOLATED throwaway database, and asserts the ledger reconciles (no
// I1–I4 drift). It's the only check that the demo/pentest seed — gitignored and
// built on the fly by `task seed:demo` — actually applies against the live schema
// and leaves the balance_minor / held_minor caches consistent with the append-only
// ledger. Skips without TEST_DATABASE_DSN, like the rest of this suite.
func TestDemoSeedLoadsCleanAndReconciles(t *testing.T) {
	if testDSN == "" {
		t.Skip("set TEST_DATABASE_DSN to run DB integration tests")
	}
	ctx := context.Background()

	// 1. Generate a deterministic, modest-sized seed. -seed fixes the RNG so a
	//    failure reproduces; the sizes keep CI fast while still exercising the bulk
	//    insert path + the real money functions the seed posts activity through.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	seedFile := filepath.Join(t.TempDir(), "seed_demo.sql")
	gen := exec.CommandContext(ctx, "go", "run", "./db/seedgen",
		"-out", seedFile, "-seed", "1", "-users", "60", "-accounts", "150", "-txns", "300")
	gen.Dir = root
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("seedgen: %v\n%s", err, out)
	}
	seedSQL, err := os.ReadFile(seedFile)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	// 2. Create an isolated database so this never pollutes the shared bank0_test the
	//    other integration tests use. CREATE/DROP DATABASE can't run on a connection
	//    to the target, so issue them over the base (testDSN) connection. FORCE drops
	//    any lingering connection from a previous run.
	const seedDB = "bank0_seed_test"
	base := newRawPG(t, testDSN)
	if _, err := base.Pool.Exec(ctx, `DROP DATABASE IF EXISTS `+seedDB+` WITH (FORCE)`); err != nil {
		t.Fatalf("drop seed db: %v", err)
	}
	if _, err := base.Pool.Exec(ctx, `CREATE DATABASE `+seedDB); err != nil {
		t.Fatalf("create seed db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = base.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+seedDB+` WITH (FORCE)`)
		base.Close()
	})

	seedDSN := swapDBName(testDSN, seedDB)

	// 3. Migrate the fresh DB, then load the seed. seedgen emits one PL/pgSQL DO
	//    block, so the whole seed loads in a single Exec.
	if err := migrate.Up(seedDSN); err != nil {
		t.Fatalf("migrate seed db: %v", err)
	}
	pg := newRawPG(t, seedDSN)
	t.Cleanup(pg.Close) // runs before the DROP above (cleanups are LIFO)
	if _, err := pg.Pool.Exec(ctx, string(seedSQL)); err != nil {
		t.Fatalf("load demo seed: %v", err)
	}

	// 4. The seed funds and moves money through the real functions, so the ledger
	//    MUST reconcile (caches == SUM(ledger) / SUM(active holds)).
	issues, err := pg.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("demo seed left the ledger inconsistent: %d drift row(s): %+v", len(issues), issues)
	}
}

func newRawPG(t *testing.T, dsn string) *Postgres {
	t.Helper()
	pg, err := NewPostgres(config.DatabaseConfig{
		DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect %s: %v", dsn, err)
	}
	return pg
}

// swapDBName returns dsn with its database (path) replaced by name.
func swapDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}
