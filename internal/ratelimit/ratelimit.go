// Package ratelimit caps how often an unauthenticated caller may write.
//
// It exists in Phase 5 rather than Phase 7 with the rest of the hardening for
// one reason: order creation and waiter calls are the first endpoints that let
// someone with no account insert rows. Idempotency stops an honest phone
// double-tapping; it does nothing about a bored teenager holding down a button.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Per-minute allowances for the guest write endpoints. Both are generous for a
// real table and useless for a script: a group ordering separately might place
// several orders in a minute, but not ten.
const (
	OrdersPerMinute = 10
	CallsPerMinute  = 5
)

// Window is the bucket length. A fixed window, not a sliding one — the known
// cost is that a caller can fit up to 2×limit across a window boundary, which
// for "ten orders a minute" means twenty in a bad second and is still nowhere
// near a problem. A sliding window would need a sorted set per key and a
// trimming pass on every request, which is a lot of Redis for a limit whose job
// is to stop nonsense rather than to meter a paid API.
const Window = time.Minute

type Limiter struct {
	redis  *redis.Client
	logger *slog.Logger
}

// New returns a limiter. A nil client is allowed and means "no limiting",
// which is what a deployment without Redis gets.
func New(rdb *redis.Client, logger *slog.Logger) *Limiter {
	return &Limiter{redis: rdb, logger: logger}
}

type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Allow consumes one unit against key.
//
// It fails **open**: if Redis is unreachable the request is allowed. That is
// deliberate and it is the right way round for this limiter. Redis is already
// treated as a cache rather than a source of truth everywhere else in this
// service, and the failure being prevented — a guest spamming the order button
// — is much less bad than the alternative, which is a restaurant unable to take
// any orders at all because a cache is down.
func (l *Limiter) Allow(ctx context.Context, key string, limit int) Result {
	if l == nil || l.redis == nil || limit <= 0 {
		return Result{Allowed: true, Remaining: limit}
	}

	// The window number is part of the key, so expiry does the cleanup and
	// there is no reset timestamp to store or race on.
	now := time.Now()
	slot := now.UnixNano() / int64(Window)
	redisKey := fmt.Sprintf("rl:%s:%d", key, slot)

	pipe := l.redis.TxPipeline()
	count := pipe.Incr(ctx, redisKey)
	// Set on every request rather than only the first: an INCR that lands
	// without an expiry — because the process died between the two commands on
	// an earlier request — would otherwise leave a key that never resets and a
	// caller locked out forever.
	pipe.Expire(ctx, redisKey, Window+time.Second)

	if _, err := pipe.Exec(ctx); err != nil {
		l.logger.WarnContext(ctx, "rate limiter unavailable, allowing request",
			slog.String("key", key), slog.Any("error", err))
		return Result{Allowed: true, Remaining: limit}
	}

	used := int(count.Val())
	if used > limit {
		// How long until this window ends, which is when the caller may retry.
		elapsed := time.Duration(now.UnixNano() % int64(Window))
		return Result{Allowed: false, RetryAfter: Window - elapsed}
	}
	return Result{Allowed: true, Remaining: limit - used}
}
