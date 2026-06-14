// Package db wires the pgx pool and the sqlc-generated Queries, plus a few
// hand-written calls for set-returning PL/pgSQL functions (see bank.go).
package db

//go:generate sqlc generate -f ../../db/sqlc.yaml

import (
	"context"
	"fmt"
	"time"

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
	// Be explicit about the connection lifecycle rather than leaning on pgx's
	// defaults: recycle connections hourly (with jitter so a whole pool doesn't
	// reconnect at once), reap idle ones above MinConns, and health-check
	// periodically. Crucially set a per-dial connect timeout — without it a hung TCP
	// dial to Postgres during normal operation (not just the startup Ping below) can
	// block a request indefinitely waiting to acquire a connection.
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnLifetimeJitter = 5 * time.Minute
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnTimeout

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
