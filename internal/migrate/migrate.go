// Package migrate runs the embedded goose migrations against a database using
// the pgx stdlib adapter. Used by the `bank0 migrate` subcommand (Helm hook Job)
// and by the auto-migrate path for local docker-compose.
package migrate

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" sql driver
	"github.com/pressly/goose/v3"

	migrations "github.com/minhtt159/bank0/db/migrations"
)

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
