// Package db wires the pgx pool and the sqlc-generated Queries, plus a few
// hand-written calls for set-returning PL/pgSQL functions (see bank.go).
package db

//go:generate sqlc generate -f ../../db/sqlc.yaml

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhtt159/bank0/internal/config"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Postgres bundles the connection pool and the typed query interface.
type Postgres struct {
	Pool    *pgxpool.Pool
	Queries *sqlc.Queries
}

func NewPostgres(cfg config.DatabaseConfig) (*Postgres, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnTimeout)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	poolCfg.MaxConns = int32(cfg.MaxOpenConns)
	poolCfg.MinConns = int32(cfg.MaxIdleConns)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &Postgres{Pool: pool, Queries: sqlc.New(pool)}, nil
}

func (p *Postgres) Close() {
	if p.Pool != nil {
		p.Pool.Close()
	}
}
