// Package postgres owns everything that talks to the database: the connection
// pool, migrations, and (from Phase 1 onward) the repository implementations
// that satisfy the interfaces declared in internal/domain.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ubilolxon/qrmenu/internal/config"
)

// Connect builds a pool and verifies it can actually reach the database.
// A pool that has never been pinged reports healthy while every query fails.
func Connect(ctx context.Context, cfg config.DB) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// Only override what the caller actually set. ParseConfig has already
	// filled in sane defaults, and blindly assigning a zero value is worse than
	// leaving it alone — a zero MaxConnLifetime marks every connection expired
	// the moment it is created, and the pool fails with an obscure "too many
	// failed attempts acquiring connection" rather than anything about config.
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	poolCfg.HealthCheckPeriod = time.Minute

	// Scope every connection to the tenant of the request acquiring it, so the
	// row-level security policies in migration 00008 have something to act on.
	// See internal/postgres/rls.go for why this runs on every acquire and not
	// only the scoped ones.
	poolCfg.BeforeAcquire = setTenantGUC

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
