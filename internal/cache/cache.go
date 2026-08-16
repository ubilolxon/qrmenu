// Package cache wraps the Redis client used for the public menu cache and,
// from Phase 7, rate limiting.
//
// Redis is a performance dependency, not a correctness one: a cache miss must
// always fall through to Postgres. Nothing here may be on the path of a write.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ubilolxon/qrmenu/internal/config"
)

// New builds the client. It cannot fail: go-redis dials lazily and reconnects
// on its own, so the client is constructed unconditionally and stays usable
// even if Redis is down at boot and comes back later.
//
// Discarding the client on a failed startup ping would be worse than useless —
// the process would run cacheless until someone restarted it, long after Redis
// had recovered.
func New(cfg config.Redis) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
}

// Ping verifies reachability. Callers use it to log a startup warning, never
// to decide whether to keep the client.
func Ping(ctx context.Context, client *redis.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	return nil
}
