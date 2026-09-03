// Package ratelimit caps how often an unauthenticated caller may write.
//
// It exists in Phase 5 rather than Phase 7 with the rest of the hardening for
// one reason: order creation and waiter calls are the first endpoints that let
// someone with no account insert rows. Idempotency stops an honest phone
// double-tapping; it does nothing about a bored teenager holding down a button.
package ratelimit

import (
	"context"
	"errors"
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

	// PublicMenuPerMinute caps menu reads per IP. High, because a real table of
	// six people scanning and re-scanning is a dozen requests in a minute and
	// the response is cached anyway; the number that matters is that it is not
	// unbounded.
	PublicMenuPerMinute = 60

	// Login is limited per IP *and* per email address, and the pair is the
	// point. Per IP alone does nothing against credential stuffing from a
	// botnet; per email alone does nothing against one host walking a list of
	// addresses. Only failures count against these — see Peek and Hit.
	LoginPerIPPerMinute    = 15
	LoginPerEmailPerMinute = 5
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

// Peek reports whether key is already over its limit, without consuming
// anything.
//
// It exists for login, where consuming on every attempt would mean a user who
// signs in on their phone and their laptop and then their phone again has spent
// part of a budget meant for an attacker. Only failures should count, which
// takes a check before and a Hit after.
func (l *Limiter) Peek(ctx context.Context, key string, limit int) Result {
	if l == nil || l.redis == nil || limit <= 0 {
		return Result{Allowed: true, Remaining: limit}
	}

	now := time.Now()
	used, err := l.redis.Get(ctx, windowKey(key, now)).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		l.logger.WarnContext(ctx, "rate limiter unavailable, allowing request",
			slog.String("key", key), slog.Any("error", err))
		return Result{Allowed: true, Remaining: limit}
	}

	if used >= limit {
		return Result{Allowed: false, RetryAfter: remaining(now)}
	}
	return Result{Allowed: true, Remaining: limit - used}
}

// Hit records one unit against key. The result is ignored by callers that only
// want to count — Peek is what asks the question.
func (l *Limiter) Hit(ctx context.Context, key string) {
	if l == nil || l.redis == nil {
		return
	}

	pipe := l.redis.TxPipeline()
	redisKey := windowKey(key, time.Now())
	pipe.Incr(ctx, redisKey)
	pipe.Expire(ctx, redisKey, Window+time.Second)

	if _, err := pipe.Exec(ctx); err != nil {
		l.logger.WarnContext(ctx, "rate limiter unavailable, attempt not counted",
			slog.String("key", key), slog.Any("error", err))
	}
}

// windowKey folds the window number into the key, so expiry does the cleanup
// and there is no reset timestamp to store or race on.
func windowKey(key string, now time.Time) string {
	return fmt.Sprintf("rl:%s:%d", key, now.UnixNano()/int64(Window))
}

// remaining is how long until the current window ends, which is when a blocked
// caller may retry.
func remaining(now time.Time) time.Duration {
	return Window - time.Duration(now.UnixNano()%int64(Window))
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

	now := time.Now()
	redisKey := windowKey(key, now)

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
		return Result{Allowed: false, RetryAfter: remaining(now)}
	}
	return Result{Allowed: true, Remaining: limit - used}
}
