// Command api runs the QR menu HTTP API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/cache"
	"github.com/ubilolxon/qrmenu/internal/config"
	apphttp "github.com/ubilolxon/qrmenu/internal/http"
	"github.com/ubilolxon/qrmenu/internal/http/handler"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/service"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if config failed, so report to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Optional: absent in production, where config comes from the environment.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	version := buildVersion()
	logger.Info("starting qrmenu api",
		slog.String("version", version),
		slog.String("env", cfg.Env),
	)

	// Cancelled on SIGINT/SIGTERM; this context is what drives graceful
	// shutdown all the way down to in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Connect(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer db.Close()
	logger.Info("connected to postgres", slog.Int("max_conns", int(cfg.DB.MaxConns)))

	// Redis is a cache, not a source of truth, so an unreachable Redis warns
	// instead of aborting startup — the menu cache simply misses and falls
	// through to Postgres. The client is kept either way; go-redis reconnects
	// on its own once Redis comes back.
	rdb := cache.New(cfg.Redis)
	defer func() { _ = rdb.Close() }()

	if err := cache.Ping(ctx, rdb); err != nil {
		logger.Warn("redis unreachable at startup, serving without cache until it recovers",
			slog.String("addr", cfg.Redis.Addr),
			slog.Any("error", err),
		)
	} else {
		logger.Info("connected to redis", slog.String("addr", cfg.Redis.Addr))
	}

	store := postgres.NewStore(db)
	issuer := auth.NewIssuer(cfg.Auth.JWTSecret, cfg.Auth.AccessTTL, cfg.Auth.Issuer)

	authSvc, err := service.NewAuth(store, issuer, cfg.Auth.RefreshTTL, logger)
	if err != nil {
		return fmt.Errorf("build auth service: %w", err)
	}

	publicPages, err := handler.NewPublic(service.NewPublicMenu(store, rdb, logger), cfg.Public)
	if err != nil {
		return fmt.Errorf("build public handler: %w", err)
	}

	router := apphttp.NewRouter(apphttp.Deps{
		Config:      cfg,
		Logger:      logger,
		DB:          db,
		Redis:       rdb,
		Version:     version,
		Issuer:      issuer,
		AuthSvc:     authSvc,
		Restaurants: service.NewRestaurant(store),
		Menus:       service.NewMenu(store),
		Tables:      service.NewTable(store),
		PublicPages: publicPages,
	})

	return apphttp.Serve(ctx, cfg.HTTP, logger, router)
}

// newLogger emits JSON in deployed environments so logs are queryable, and
// human-readable text locally.
func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}

	var h slog.Handler
	if cfg.IsDev() {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h).With(slog.String("service", "qrmenu-api"))
}

// buildVersion reports the VCS revision stamped into the binary by the Go
// toolchain, so a running instance can say exactly which commit it is.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	return rev + modified
}
