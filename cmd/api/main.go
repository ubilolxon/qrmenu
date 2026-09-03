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
	"time"

	"github.com/joho/godotenv"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/billing"
	"github.com/ubilolxon/qrmenu/internal/billing/payme"
	"github.com/ubilolxon/qrmenu/internal/cache"
	"github.com/ubilolxon/qrmenu/internal/config"
	apphttp "github.com/ubilolxon/qrmenu/internal/http"
	"github.com/ubilolxon/qrmenu/internal/http/handler"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/ratelimit"
	"github.com/ubilolxon/qrmenu/internal/realtime"
	"github.com/ubilolxon/qrmenu/internal/service"
	"github.com/ubilolxon/qrmenu/internal/storage"
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

	// Object storage holds uploaded images. Like Redis it is not required to
	// boot: without it the API refuses uploads and the menu serves without
	// photos, which is a worse menu but a working one.
	files, err := storage.New(cfg.Storage)
	if err != nil {
		return fmt.Errorf("connect storage: %w", err)
	}
	if err := files.EnsureBucket(ctx, cfg.Storage.Region); err != nil {
		logger.Warn("object storage is not ready; image uploads will fail until it is",
			slog.String("bucket", files.Bucket()), slog.Any("error", err))
	} else {
		logger.Info("object storage ready", slog.String("bucket", files.Bucket()))
	}

	store := postgres.NewStore(db)
	issuer := auth.NewIssuer(cfg.Auth.JWTSecret, cfg.Auth.AccessTTL, cfg.Auth.Issuer)

	authSvc, err := service.NewAuth(store, issuer, cfg.Auth.RefreshTTL, logger)
	if err != nil {
		return fmt.Errorf("build auth service: %w", err)
	}

	// Plans is consulted on every write path that a plan puts a ceiling on, so
	// it is built before the services that hold it.
	plans := service.NewPlan(store)

	// The billing service is also the ledger every payment provider talks to,
	// and a provider needs that ledger to be constructed. So the service comes
	// first and the registry is handed to it afterwards.
	billingSvc := service.NewBilling(store, logger)
	billingSvc.UseProviders(paymentProviders(cfg, billingSvc, logger))

	images := service.NewImage(store, files, plans, logger)

	// One hub per process. Every staff device connected to this instance
	// subscribes to it; see internal/realtime for what running more than one
	// instance would take.
	hub := realtime.New()
	orders := service.NewOrder(store, hub, plans, logger)

	publicPages, err := handler.NewPublic(service.NewPublicMenu(store, rdb, logger, files), cfg.Public)
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
		Restaurants: service.NewRestaurant(store, plans),
		Menus:       service.NewMenu(store, plans),
		Tables:      service.NewTable(store, plans),
		Images:      images,
		Orders:      orders,
		Plans:       plans,
		BillingSvc:  billingSvc,
		Files:       files,
		Limiter:     ratelimit.New(rdb, logger),
		PublicPages: publicPages,
	})

	// Presigned uploads that are never completed would otherwise accumulate as
	// pending rows and orphaned objects, on a bill that only ever grows.
	go sweepAbandonedUploads(ctx, images, logger)

	// Subscriptions expire on a clock, not on a request, so something has to
	// notice. This is that something.
	go sweepSubscriptions(ctx, billingSvc, cfg.Billing, logger)

	return apphttp.Serve(ctx, cfg.HTTP, logger, router)
}

// paymentProviders builds the set this deployment has credentials for.
//
// Manual is always present: every payment system eventually needs a human
// override, and having one from the start means the first customer can be
// onboarded before any provider integration is finished. Everything else
// appears only when configured, so a missing secret means the method is not
// offered rather than offered and broken.
func paymentProviders(cfg config.Config, ledger billing.Ledger, logger *slog.Logger) *billing.Registry {
	providers := []billing.Provider{
		billing.Manual{Instructions: cfg.Billing.ManualInstructions},
	}

	if cfg.Billing.Payme.Configured() {
		providers = append(providers, payme.New(payme.Config{
			MerchantID:  cfg.Billing.Payme.MerchantID,
			Key:         cfg.Billing.Payme.Key,
			CheckoutURL: cfg.Billing.Payme.CheckoutURL,
		}, ledger, logger))
		logger.Info("payme provider configured",
			slog.String("merchant_id", cfg.Billing.Payme.MerchantID))
	} else {
		logger.Info("payme is not configured; payment is by manual settlement only")
	}

	return billing.NewRegistry(providers...)
}

// sweepSubscriptions runs the expiry pass until shutdown.
//
// It runs once at startup as well as on the tick, because a deployment that was
// down over a weekend would otherwise leave every lapsed subscription active
// until the next interval came round.
//
// As with the upload sweep, every instance runs it. The statements are
// idempotent — they only ever move a row out of the state they select on — so
// two instances sweeping at once do the work once between them.
func sweepSubscriptions(ctx context.Context, svc *service.Billing, cfg config.Billing, logger *slog.Logger) {
	run := func() {
		result, err := svc.Sweep(ctx, cfg.GracePeriod)
		if err != nil {
			logger.Warn("subscription sweep failed", slog.Any("error", err))
			return
		}
		if result.Expired > 0 || result.Suspended > 0 {
			logger.Info("subscription sweep",
				slog.Int("expired", result.Expired), slog.Int("suspended", result.Suspended))
		}
	}
	run()

	ticker := time.NewTicker(cfg.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// SweepInterval is how often abandoned uploads are cleared. Nothing depends on
// it being prompt — the cost of a late sweep is a few kilobytes — so it runs
// rarely enough to be invisible.
const SweepInterval = time.Hour

// sweepAbandonedUploads runs until shutdown. Running it in every instance is
// intentional: the work is idempotent, and a leader election for a garbage
// collector would be more machinery than the problem deserves.
func sweepAbandonedUploads(ctx context.Context, images *service.Image, logger *slog.Logger) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := images.SweepStale(ctx)
			if err != nil {
				logger.Warn("upload sweep failed", slog.Any("error", err))
				continue
			}
			if n > 0 {
				logger.Info("cleared abandoned uploads", slog.Int("count", n))
			}
		}
	}
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
