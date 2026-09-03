// Package middleware holds the cross-cutting HTTP concerns applied to every
// request: request identity, structured logging, panic recovery, and CORS.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/ratelimit"
)

type ctxKey int

// One block for every context key in this package. Splitting them across files
// is how two of them end up with the same value: ctxKey is an int, and a second
// file writing `= 1` collides silently with whatever iota already produced.
const (
	requestIDKey ctxKey = iota
	scopeKey
	identityKey
	drainKey
)

// WithDraining marks ctx with a channel that is closed when the server begins
// shutting down. The HTTP server sets it on every request's base context.
func WithDraining(ctx context.Context, draining <-chan struct{}) context.Context {
	return context.WithValue(ctx, drainKey, draining)
}

// Draining returns a channel closed when shutdown begins, or nil when the
// request was not served by a shutting-down-aware server. A nil channel blocks
// forever in a select, which is the right default: a handler that cannot learn
// about shutdown should simply keep working.
//
// This exists because "graceful shutdown" means two opposite things depending
// on the handler. An ordinary request should be allowed to *finish* — a POST
// halfway through writing an order must not be cut off, or its client is left
// not knowing whether the order exists. A stream held open for the length of a
// shift should be closed *immediately*, because waiting for it to finish means
// waiting out the shutdown timeout and then killing it anyway.
//
// The obvious implementation — deriving every request context from the process
// context — gets the second case right and the first one wrong. So the two
// signals are kept apart: request contexts are independent of shutdown, and
// this channel is how the handlers that care find out.
func Draining(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(drainKey).(<-chan struct{})
	return ch
}

// scope is a mutable per-request holder, and it exists because of an ordering
// problem: Logger has to run outermost to time and record everything, including
// requests that never reach a handler — but the caller's identity is not known
// until Authenticate runs, several layers in, and Authenticate publishes it by
// deriving a *new* context that the outer Logger will never see.
//
// So Logger installs a pointer here on the way in and reads it on the way out.
// One request is handled by one goroutine between those two points, so a plain
// struct is enough; nothing else may retain it.
type scope struct {
	tenantID string
	userID   string
	role     string
}

func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey).(*scope)
	return s
}

// recordActor is called by Authenticate once the token has been verified, so
// every log line for the rest of the request carries who made it. A request
// that fails authentication records nothing, which is correct — there is no
// verified actor to name.
func recordActor(ctx context.Context, id Identity) {
	if s := scopeFrom(ctx); s != nil {
		s.tenantID = id.TenantID.String()
		s.userID = id.UserID.String()
		s.role = string(id.Role)
	}
}

const RequestIDHeader = "X-Request-ID"

// RequestIDFrom returns the request ID carried on ctx, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestID attaches an ID to every request and echoes it back on the response
// so a user-reported failure can be traced to exact log lines.
//
// An inbound ID is only trusted from an internal caller behind the proxy; it is
// echoed as-is but never used for anything security-sensitive.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 64 {
			id = newID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func newID() string {
	var b [12]byte
	// crypto/rand.Read never fails on supported platforms; it panics internally
	// on entropy failure, so there is no error path worth handling here.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusRecorder captures what was actually written, since http.ResponseWriter
// exposes neither the status code nor the byte count after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController, which is
// what the SSE handler in Phase 5 uses to flush.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Logger emits one structured line per request. Health checks are logged at
// debug level so a one-second probe interval does not bury real traffic.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			actor := &scope{}
			r = r.WithContext(context.WithValue(r.Context(), scopeKey, actor))

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}

			level := slog.LevelInfo
			switch {
			case isProbe(r.URL.Path):
				level = slog.LevelDebug
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}

			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("took", time.Since(start).Round(time.Microsecond)),
				slog.String("ip", ClientIP(r)),
			}
			// Only present on an authenticated request, and deliberately
			// omitted rather than empty on the others: a blank tenant_id in a
			// log aggregator is a value that groups, and thousands of anonymous
			// menu reads would form a convincing-looking tenant.
			if actor.tenantID != "" {
				attrs = append(attrs,
					slog.String("tenant_id", actor.tenantID),
					slog.String("user_id", actor.userID),
					slog.String("role", actor.role),
				)
			}

			logger.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// ClientIP prefers the proxy-provided address. This is only ever used for logs
// and rate limiting; both headers are trivially spoofable if the service is
// exposed without a trusted proxy in front of it.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	// net.SplitHostPort, not a Cut on ":". An IPv6 RemoteAddr is "[::1]:54321",
	// so cutting at the first colon yields "[" — and since this value is a rate
	// limiter key, every IPv6 client would share one bucket and any one of them
	// could lock out the rest.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port at all, which is what a Unix socket or a hand-built test
		// request looks like. The address is already the host.
		return r.RemoteAddr
	}
	return host
}

// Recover turns a panic into a 500 rather than a dropped connection, and logs
// the stack so the bug is diagnosable.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// The client aborting mid-write surfaces as a panic in some
			// handlers; there is nobody left to respond to.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "panic recovered",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("path", r.URL.Path),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)
			httperr.Write(w, http.StatusInternalServerError, httperr.CodeInternal,
				"Something went wrong on our side.", nil)
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS answers preflights and echoes only explicitly allowed origins.
//
// The reflected-origin form is required because the admin API sends the
// refresh cookie with credentials, and browsers reject "*" on credentialed
// requests. Config rejects "*" outside dev for the same reason.
func CORS(allowed []string) func(http.Handler) http.Handler {
	allowAll := slices.Contains(allowed, "*")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowAll || slices.Contains(allowed, origin)) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Add("Vary", "Origin")

				if r.Method == http.MethodOptions {
					h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers",
						"Authorization, Content-Type, "+RequestIDHeader+", Idempotency-Key, Accept-Language")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit caps how often one caller may hit a route group.
//
// It is middleware rather than a per-handler check because the public menu is
// served by several handlers — HTML, JSON, three QR formats — and a limit that
// has to be remembered at each of them is a limit that will be forgotten at one.
//
// key names what is being limited, and it is a function rather than a value so
// the caller decides whether that is an IP, a table token, or something else.
// Like the limiter itself, this fails open: an unreachable Redis must not stop a
// restaurant serving its menu.
func RateLimit(limiter *ratelimit.Limiter, limit int, prefix string, key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			result := limiter.Allow(r.Context(), prefix+":"+key(r), limit)
			if !result.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds()+0.999)))
				httperr.Write(w, http.StatusTooManyRequests, httperr.CodeRateLimited,
					"Too many requests. Please wait a moment.", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
