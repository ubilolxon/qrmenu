// Package http wires the router and the server. It is the only package allowed
// to know about both transport concerns and the application's services.
package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/handler"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/service"
)

type Deps struct {
	Config  config.Config
	Logger  *slog.Logger
	DB      *pgxpool.Pool
	Redis   *redis.Client
	Version string

	Issuer      *auth.Issuer
	AuthSvc     *service.Auth
	Restaurants *service.Restaurant
	Menus       *service.Menu
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

		r.Route("/admin", func(r chi.Router) {
			d.mountAdmin(r)
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

// mountAdmin builds the authenticated management API.
//
// Everything except the four auth entry points sits inside the Authenticate
// group, so a route added to the wrong block fails closed — an unauthenticated
// request reaches no handler at all rather than one that forgot to check.
func (d Deps) mountAdmin(r chi.Router) {
	authHandler := handler.NewAuth(d.AuthSvc, d.Config.Auth)
	restaurants := handler.NewRestaurant(d.Restaurants)

	// Unauthenticated: these are how a caller obtains credentials in the first
	// place. Refresh and logout authenticate with the HttpOnly cookie.
	r.Route("/auth", func(r chi.Router) {
		r.Post("/register", authHandler.Register)
		r.Post("/login", authHandler.Login)
		r.Post("/refresh", authHandler.Refresh)
		r.Post("/logout", authHandler.Logout)
	})

	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticate(d.Issuer))

		r.Get("/me", authHandler.Me)

		r.Route("/restaurants", func(r chi.Router) {
			// Managers run the menus; owners additionally control how many
			// restaurants exist, since that is what a plan is billed on.
			r.With(middleware.RequireRole(domain.RoleOwner, domain.RoleManager)).
				Get("/", restaurants.List)
			r.With(middleware.RequireRole(domain.RoleOwner)).
				Post("/", restaurants.Create)

			r.Route("/{restaurantID}", func(r chi.Router) {
				r.With(middleware.RequireRole(domain.RoleOwner, domain.RoleManager)).
					Get("/", restaurants.Get)
				r.With(middleware.RequireRole(domain.RoleOwner, domain.RoleManager)).
					Patch("/", restaurants.Update)
				r.With(middleware.RequireRole(domain.RoleOwner)).
					Delete("/", restaurants.Delete)

				mountMenu(r, handler.NewMenu(d.Menus))
			})
		})
	})
}

// mountMenu hangs the whole menu tree under a restaurant.
//
// Collections that need a parent are nested under it (a category belongs to a
// menu); everything else is addressed directly by id under the restaurant,
// because a category id is already unique and forcing callers to remember the
// full ancestor chain buys nothing.
func mountMenu(r chi.Router, h handler.Menu) {
	// Menu editing is a manager's job. Owners can do it too, since owner is not
	// a superset by default — every route lists the roles it accepts.
	editors := middleware.RequireRole(domain.RoleOwner, domain.RoleManager)

	// Marking a dish sold out happens mid-service, so kitchen and waiting staff
	// can do it without being able to change prices or anything else.
	stock := middleware.RequireRole(
		domain.RoleOwner, domain.RoleManager, domain.RoleKitchen, domain.RoleWaiter)

	r.Route("/menus", func(r chi.Router) {
		r.With(editors).Get("/", h.ListMenus)
		r.With(editors).Post("/", h.CreateMenu)
		// Registered before /{menuID} so "reorder" is not read as an id.
		r.With(editors).Put("/reorder", h.ReorderMenus)

		r.Route("/{menuID}", func(r chi.Router) {
			r.With(editors).Get("/", h.GetMenu)
			r.With(editors).Patch("/", h.UpdateMenu)
			r.With(editors).Delete("/", h.DeleteMenu)

			r.With(editors).Get("/categories", h.ListCategories)
			r.With(editors).Post("/categories", h.CreateCategory)
		})
	})

	r.Route("/categories", func(r chi.Router) {
		r.With(editors).Put("/reorder", h.ReorderCategories)

		r.Route("/{categoryID}", func(r chi.Router) {
			r.With(editors).Get("/", h.GetCategory)
			r.With(editors).Patch("/", h.UpdateCategory)
			r.With(editors).Delete("/", h.DeleteCategory)

			r.With(editors).Get("/items", h.ListItems)
			r.With(editors).Post("/items", h.CreateItem)
		})
	})

	r.Route("/items", func(r chi.Router) {
		r.With(editors).Put("/reorder", h.ReorderItems)

		r.Route("/{itemID}", func(r chi.Router) {
			r.With(editors).Get("/", h.GetItem)
			r.With(editors).Patch("/", h.UpdateItem)
			r.With(editors).Delete("/", h.DeleteItem)
			r.With(stock).Put("/availability", h.SetAvailability)

			r.With(editors).Get("/variants", h.ListVariants)
			r.With(editors).Post("/variants", h.CreateVariant)
			r.With(editors).Get("/modifier-groups", h.ListModifierGroups)
			r.With(editors).Post("/modifier-groups", h.CreateModifierGroup)
		})
	})

	r.Route("/variants/{variantID}", func(r chi.Router) {
		r.With(editors).Patch("/", h.UpdateVariant)
		r.With(editors).Delete("/", h.DeleteVariant)
	})

	r.Route("/modifier-groups/{groupID}", func(r chi.Router) {
		r.With(editors).Patch("/", h.UpdateModifierGroup)
		r.With(editors).Delete("/", h.DeleteModifierGroup)

		r.With(editors).Get("/modifiers", h.ListModifiers)
		r.With(editors).Post("/modifiers", h.CreateModifier)
	})

	r.Route("/modifiers/{modifierID}", func(r chi.Router) {
		r.With(editors).Patch("/", h.UpdateModifier)
		r.With(editors).Delete("/", h.DeleteModifier)
	})
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	httperr.Write(w, http.StatusNotImplemented, "NOT_IMPLEMENTED",
		"This endpoint is not implemented yet.", nil)
}
