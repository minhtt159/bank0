// Package migrate runs the embedded goose migrations against a database using
// the pgx stdlib adapter. Used by the `bank0 migrate` subcommand (Helm hook Job)
// and by the auto-migrate path for local docker-compose.
package migrate

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" sql driver
	"github.com/pressly/goose/v3"

	migrations "github.com/minhtt159/bank0/db/migrations"
)

// migrationLockKey is an arbitrary, fixed 64-bit key. Every process that runs
// bank0 migrations contends on this one advisory lock, so concurrent migrators
// serialize instead of racing on shared DDL (e.g. two `CREATE TYPE`/`CREATE
// TABLE` colliding on the pg_type/pg_class catalog, or one connection reading
// goose_db_version before another has created it). This happens with the
// parallel integration-test packages against one TEST_DATABASE_DSN, and guards
// the production migrate Job against an overlapping run. goose's classic
// Up/Down take no lock of their own, so this is purely additive.
const migrationLockKey int64 = 0x62616e6b30303031 // "bank0001"

func open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func Up(dsn string) error {
	db, err := open(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	// Hold a session-level advisory lock for the whole migration so concurrent
	// migrators block here rather than colliding on catalog DDL. The lock lives
	// on a dedicated connection; goose runs on the pool. Closing the connection
	// releases the lock even if the explicit unlock is missed.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey) //nolint:errcheck

	return goose.Up(db, ".")
}

func Down(dsn string) error {
	db, err := open(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	return goose.Down(db, ".")
}

func Status(dsn string) error {
	db, err := open(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	return goose.Status(db, ".")
}
