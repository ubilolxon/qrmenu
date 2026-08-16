// Package middleware holds the cross-cutting HTTP concerns applied to every
// request: request identity, structured logging, panic recovery, and CORS.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/ubilolxon/qrmenu/internal/httperr"
)

type ctxKey int

const requestIDKey ctxKey = iota

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

			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("took", time.Since(start).Round(time.Microsecond)),
				slog.String("ip", clientIP(r)),
			)
		})
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// clientIP prefers the proxy-provided address. This is only ever used for logs
// and rate limiting; both headers are trivially spoofable if the service is
// exposed without a trusted proxy in front of it.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
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
