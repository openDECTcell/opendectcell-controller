// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db opens the PostgreSQL pool, applies the embedded schema
// migrations (docs/schema.md) and manages the data keys that encrypt
// secrets at rest.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to PostgreSQL and checks the connection.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx's message can include the URL; don't pass it on.
		return nil, fmt.Errorf("db: invalid database URL")
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: cannot reach database: %w", err)
	}
	return pool, nil
}
