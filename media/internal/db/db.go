// Package db opens the media subsystem's Postgres pool and puts its schema in
// place.
//
// The schema is applied with CREATE ... IF NOT EXISTS on every start rather
// than through a migration tool: this is one service that owns its own two
// tables, and a start-up that cannot reach its database must fail loudly at
// start-up rather than at the first upload.
package db

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Open connects and verifies the connection. `dsn` is a libpq URL or key/value
// string; there is no default, because a service that silently connected to
// "localhost" would be a service that silently wrote to the wrong database.
func Open(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("no database DSN: set --dsn or AURA_MEDIA_DSN")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("bad DSN: %w", err)
	}
	if maxConns > 0 {
		// One connection per worker, plus headroom for the API and the
		// reclaimer. A pool smaller than the worker count turns SKIP LOCKED's
		// parallelism into a queue for connections.
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable: %w", err)
	}
	return pool, nil
}

// Migrate applies the schema. Safe to run concurrently from several processes:
// every statement is IF NOT EXISTS, and Postgres serialises the DDL.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Schema is the DDL this service applies, exposed so `aura-media migrate
// --print` can show an operator exactly what will run against their database
// before it does.
func Schema() string { return schema }
