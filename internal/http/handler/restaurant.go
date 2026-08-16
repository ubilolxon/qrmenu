package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/service"
)

type Restaurant struct {
	svc *service.Restaurant
}

func NewRestaurant(svc *service.Restaurant) Restaurant {
	return Restaurant{svc: svc}
}

// The admin API returns every translation, not one resolved language: this is
// the editing surface, and the owner needs to see and change all of them. The
// public API in Phase 3 resolves to a single locale instead.
type restaurantResponse struct {
	ID            string         `json:"id"`
	Slug          string         `json:"slug"`
	Name          i18n.Text      `json:"name"`
	Description   i18n.Text      `json:"description"`
	Currency      string         `json:"currency"`
	Timezone      string         `json:"timezone"`
	DefaultLocale string         `json:"default_locale"`
	Locales       []string       `json:"locales"`
	Phone         *string        `json:"phone"`
	Address       *string        `json:"address"`
	Theme         map[string]any `json:"theme"`
	MenuVersion   int32          `json:"menu_version"`
	IsActive      bool           `json:"is_active"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

func toRestaurantResponse(r domain.Restaurant) restaurantResponse {
	return restaurantResponse{
		ID:            r.ID.String(),
		Slug:          r.Slug,
		Name:          r.Name,
		Description:   r.Description,
		Currency:      r.Currency,
		Timezone:      r.Timezone,
		DefaultLocale: r.DefaultLocale,
		Locales:       r.Locales,
		Phone:         r.Phone,
		Address:       r.Address,
		Theme:         r.Theme,
		MenuVersion:   r.MenuVersion,
		IsActive:      r.IsActive,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

type createRestaurantRequest struct {
	Slug          string    `json:"slug"`
	Name          i18n.Text `json:"name"`
	Description   i18n.Text `json:"description"`
	Currency      string    `json:"currency"`
	Timezone      string    `json:"timezone"`
	DefaultLocale string    `json:"default_locale"`
	Locales       []string  `json:"locales"`
	Phone         *string   `json:"phone"`
	Address       *string   `json:"address"`
}

// Pointer fields distinguish "not sent" from "sent as null or empty", which is
// the whole point of PATCH.
type updateRestaurantRequest struct {
	Slug          *string         `json:"slug"`
	Name          *i18n.Text      `json:"name"`
	Description   *i18n.Text      `json:"description"`
	Currency      *string         `json:"currency"`
	Timezone      *string         `json:"timezone"`
	DefaultLocale *string         `json:"default_locale"`
	Locales       *[]string       `json:"locales"`
	Phone         *string         `json:"phone"`
	Address       *string         `json:"address"`
	Theme         *map[string]any `json:"theme"`
	IsActive      *bool           `json:"is_active"`
}

func (h Restaurant) List(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	restaurants, err := h.svc.List(r.Context(), identity.TenantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}

	// Always an array, never null: a JSON null here forces every caller to
	// null-check before iterating.
	out := make([]restaurantResponse, len(restaurants))
	for i, rest := range restaurants {
		out[i] = toRestaurantResponse(rest)
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"restaurants": out})
}

func (h Restaurant) Create(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	var req createRestaurantRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	// Defaults applied here rather than in the database, so validation sees the
	// same values that will be stored.
	if req.Currency == "" {
		req.Currency = "UZS"
	}
	if req.Timezone == "" {
		req.Timezone = "Asia/Tashkent"
	}
	if req.DefaultLocale == "" {
		req.DefaultLocale = "uz"
	}
	if len(req.Locales) == 0 {
		req.Locales = []string{req.DefaultLocale}
	}
	if req.Description == nil {
		req.Description = i18n.Text{}
	}

	created, err := h.svc.Create(r.Context(), identity.TenantID, domain.RestaurantInput{
		Slug:          req.Slug,
		Name:          req.Name,
		Description:   req.Description,
		Currency:      req.Currency,
		Timezone:      req.Timezone,
		DefaultLocale: req.DefaultLocale,
		Locales:       req.Locales,
		Phone:         req.Phone,
		Address:       req.Address,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toRestaurantResponse(created))
}

func (h Restaurant) Get(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, err := PathUUID(r, "restaurant_id", chi.URLParam(r, "restaurantID"))
	if err != nil {
		RenderError(w, r, err)
		return
	}

	restaurant, err := h.svc.Get(r.Context(), identity.TenantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toRestaurantResponse(restaurant))
}

func (h Restaurant) Update(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, err := PathUUID(r, "restaurant_id", chi.URLParam(r, "restaurantID"))
	if err != nil {
		RenderError(w, r, err)
		return
	}

	var req updateRestaurantRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	updated, err := h.svc.Update(r.Context(), identity.TenantID, id, service.RestaurantPatch{
		Slug:          req.Slug,
		Name:          req.Name,
		Description:   req.Description,
		Currency:      req.Currency,
		Timezone:      req.Timezone,
		DefaultLocale: req.DefaultLocale,
		Locales:       req.Locales,
		Phone:         req.Phone,
		Address:       req.Address,
		Theme:         req.Theme,
		IsActive:      req.IsActive,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toRestaurantResponse(updated))
}

func (h Restaurant) Delete(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, err := PathUUID(r, "restaurant_id", chi.URLParam(r, "restaurantID"))
	if err != nil {
		RenderError(w, r, err)
		return
	}

	if err := h.svc.Delete(r.Context(), identity.TenantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}
