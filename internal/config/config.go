// Package config loads and validates all runtime configuration from the
// environment. Everything the process needs is resolved once at boot, so a
// misconfigured deployment fails immediately and loudly rather than at the
// first request that happens to touch a missing value.
package config

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string // dev | staging | prod
	LogLevel slog.Level
	HTTP     HTTP
	DB       DB
	Redis    Redis
	Storage  Storage
	Auth     Auth
	Public   Public
}

type Public struct {
	// BaseURL is what QR codes encode and what printed cards show, so it must
	// be the address a guest's phone can actually reach — not the internal
	// listen address.
	BaseURL string
	// CacheMaxAge is the browser/CDN freshness window for a public menu.
	CacheMaxAge time.Duration
	// StaleWhileRevalidate lets a CDN keep serving a slightly old menu while it
	// fetches a new one, so a cache expiry never becomes a latency spike at the
	// start of a dinner rush.
	StaleWhileRevalidate time.Duration
}

type Auth struct {
	JWTSecret  []byte
	Issuer     string
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// Refresh cookie attributes. The cookie is HttpOnly and SameSite=Strict
	// unconditionally; only these vary by environment.
	CookieName   string
	CookieDomain string
	CookiePath   string
	CookieSecure bool
}

type HTTP struct {
	Addr            string
	AllowedOrigins  []string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

type DB struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

type Redis struct {
	Addr     string
	Password string
	DB       int
}

type Storage struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
	PublicURL string // CDN or public base URL used when building image URLs
}

func (c Config) IsDev() bool { return c.Env == "dev" }

// Load reads configuration from the environment. All problems are collected
// before returning so a bad deploy reports every missing value at once instead
// of one per restart.
func Load() (Config, error) {
	var l loader

	cfg := Config{
		Env:      l.str("APP_ENV", "dev"),
		LogLevel: parseLevel(l.str("LOG_LEVEL", "info")),
		HTTP: HTTP{
			Addr:            l.str("HTTP_ADDR", ":8080"),
			AllowedOrigins:  l.csv("HTTP_ALLOWED_ORIGINS", "http://localhost:5173"),
			ReadTimeout:     l.dur("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.dur("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:     l.dur("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: l.dur("HTTP_SHUTDOWN_TIMEOUT", 30*time.Second),
		},
		DB: DB{
			URL:             l.required("DATABASE_URL"),
			MaxConns:        int32(l.intv("DB_MAX_CONNS", 20)),
			MinConns:        int32(l.intv("DB_MIN_CONNS", 2)),
			MaxConnLifetime: l.dur("DB_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: l.dur("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
		},
		Redis: Redis{
			Addr:     l.str("REDIS_ADDR", "localhost:6379"),
			Password: l.str("REDIS_PASSWORD", ""),
			DB:       l.intv("REDIS_DB", 0),
		},
		Storage: Storage{
			Endpoint:  l.str("S3_ENDPOINT", "localhost:9000"),
			AccessKey: l.str("S3_ACCESS_KEY", ""),
			SecretKey: l.str("S3_SECRET_KEY", ""),
			Bucket:    l.str("S3_BUCKET", "qrmenu"),
			Region:    l.str("S3_REGION", "us-east-1"),
			UseSSL:    l.boolv("S3_USE_SSL", false),
			PublicURL: l.str("S3_PUBLIC_URL", "http://localhost:9000/qrmenu"),
		},
		Auth: Auth{
			Issuer:     l.str("AUTH_ISSUER", "qrmenu"),
			AccessTTL:  l.dur("AUTH_ACCESS_TTL", 15*time.Minute),
			RefreshTTL: l.dur("AUTH_REFRESH_TTL", 30*24*time.Hour),

			CookieName:   l.str("AUTH_COOKIE_NAME", "qrmenu_refresh"),
			CookieDomain: l.str("AUTH_COOKIE_DOMAIN", ""),
			CookiePath:   l.str("AUTH_COOKIE_PATH", "/api/v1/admin/auth"),
		},
		Public: Public{
			BaseURL:              strings.TrimRight(l.str("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
			CacheMaxAge:          l.dur("PUBLIC_CACHE_MAX_AGE", 60*time.Second),
			StaleWhileRevalidate: l.dur("PUBLIC_STALE_WHILE_REVALIDATE", 300*time.Second),
		},
	}

	switch cfg.Env {
	case "dev", "staging", "prod":
	default:
		l.errf("APP_ENV must be one of dev|staging|prod, got %q", cfg.Env)
	}

	cfg.Auth.JWTSecret = l.jwtSecret(cfg.IsDev())
	// Secure cookies require HTTPS, which a local dev server does not have.
	cfg.Auth.CookieSecure = l.boolv("AUTH_COOKIE_SECURE", !cfg.IsDev())

	if cfg.Auth.AccessTTL > time.Hour {
		l.errf("AUTH_ACCESS_TTL is %v; access tokens are not revocable, so keep them short", cfg.Auth.AccessTTL)
	}
	if cfg.Auth.RefreshTTL <= cfg.Auth.AccessTTL {
		l.errf("AUTH_REFRESH_TTL (%v) must be longer than AUTH_ACCESS_TTL (%v)",
			cfg.Auth.RefreshTTL, cfg.Auth.AccessTTL)
	}

	// A wildcard CORS origin combined with credentialed refresh cookies would
	// let any site drive the admin API on a logged-in user's behalf.
	if !cfg.IsDev() {
		for _, o := range cfg.HTTP.AllowedOrigins {
			if o == "*" {
				l.errf("HTTP_ALLOWED_ORIGINS may not contain '*' outside dev")
			}
		}
	}

	if err := l.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loader accumulates configuration errors so Load can report all of them.
type loader struct{ problems []string }

func (l *loader) errf(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

func (l *loader) err() error {
	if len(l.problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(l.problems, "\n  - "))
}

// MinJWTSecretLen matches the HMAC-SHA256 block size. A shorter secret weakens
// the signature; a longer one is hashed down to it anyway.
const MinJWTSecretLen = 32

// jwtSecret loads the signing key.
//
// Outside dev it is required and length-checked. In dev a missing key is
// generated at random so a fresh checkout runs with no setup — the cost is that
// every restart invalidates issued tokens, which is why .env.example ships a
// fixed value.
func (l *loader) jwtSecret(isDev bool) []byte {
	raw := os.Getenv("AUTH_JWT_SECRET")

	if raw == "" {
		if !isDev {
			l.errf("AUTH_JWT_SECRET is required (at least %d characters) outside dev", MinJWTSecretLen)
			return nil
		}
		secret := make([]byte, MinJWTSecretLen)
		if _, err := rand.Read(secret); err != nil {
			l.errf("could not generate a development AUTH_JWT_SECRET: %v", err)
			return nil
		}
		slog.Warn("AUTH_JWT_SECRET is unset; generated a random one for this run. " +
			"Access tokens will stop working when the process restarts.")
		return secret
	}

	if len(raw) < MinJWTSecretLen {
		l.errf("AUTH_JWT_SECRET is %d characters, minimum is %d", len(raw), MinJWTSecretLen)
	}
	return []byte(raw)
}

func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		l.errf("%s is required", key)
	}
	return v
}

func (l *loader) csv(key, def string) []string {
	raw := l.str(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (l *loader) intv(key string, def int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.errf("%s must be an integer, got %q", key, raw)
		return def
	}
	return v
}

func (l *loader) boolv(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.errf("%s must be a boolean, got %q", key, raw)
		return def
	}
	return v
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.errf("%s must be a duration such as 30s or 5m, got %q", key, raw)
		return def
	}
	return v
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
