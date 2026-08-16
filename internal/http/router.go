// Package http wires the router and the server. It is the only package allowed
// to know about both transport concerns and the application's services.
package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/http/handler"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
)

type Deps struct {
	Config  config.Config
	Logger  *slog.Logger
	DB      *pgxpool.Pool
	Redis   *redis.Client
	Version string
}

// NewRouter builds the full route tree.
//
// The three API surfaces are mounted as separate sub-routers because they have
// genuinely different middleware: public is unauthenticated and heavily cached,
// admin requires a JWT and tenant scoping, staff holds long-lived SSE
// connections. Keeping them apart here prevents an auth middleware from being
// accidentally skipped on a route added later.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(d.Logger))
	r.Use(middleware.Recover)
	r.Use(middleware.CORS(d.Config.HTTP.AllowedOrigins))

	health := handler.Health{DB: d.DB, Redis: d.Redis, Version: d.Version}
	r.Get("/healthz", health.Live)
	r.Get("/readyz", health.Ready)

	r.Route("/api/v1", func(r chi.Router) {
		// Guests scanning a QR code. No auth; cached aggressively. (Phase 3)
		r.Route("/public", func(r chi.Router) {
			r.NotFound(notImplemented)
		})

		// Restaurant owners and managers in the SPA. JWT + tenant scope. (Phase 1)
		r.Route("/admin", func(r chi.Router) {
			r.NotFound(notImplemented)
		})

		// Waiter and kitchen devices, including the SSE stream. (Phase 5)
		r.Route("/staff", func(r chi.Router) {
			r.NotFound(notImplemented)
		})
	})

	// Payment provider callbacks. Signature-verified, never JWT-authenticated,
	// and deliberately outside /api so they are easy to exempt from CORS and
	// rate limiting. (Phase 6)
	r.Route("/webhooks", func(r chi.Router) {
		r.NotFound(notImplemented)
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httperr.NotFound(w)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		httperr.Write(w, http.StatusMethodNotAllowed, httperr.CodeBadRequest,
			"Method not allowed for this resource.", nil)
	})

	return r
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	httperr.Write(w, http.StatusNotImplemented, "NOT_IMPLEMENTED",
		"This endpoint is not implemented yet.", nil)
}
