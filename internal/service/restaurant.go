package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

type Restaurant struct {
	store *postgres.Store
	plans *Plan
}

// plans may be nil, which permits everything. See service.Plan.
func NewRestaurant(store *postgres.Store, plans *Plan) *Restaurant {
	return &Restaurant{store: store, plans: plans}
}

// Every method takes tenantID first and passes it into the query. This is the
// whole tenant-isolation strategy: the scope is not optional, not inferred from
// the payload, and not something a caller can forget to supply, because the
// signature will not compile without it.

func (s *Restaurant) Create(ctx context.Context, tenantID uuid.UUID, in domain.RestaurantInput) (domain.Restaurant, error) {
	in = normalizeRestaurantInput(in)
	if err := in.Validate(); err != nil {
		return domain.Restaurant{}, err
	}

	// Checked after validation, so a caller with a malformed payload is told
	// what is wrong with it rather than being sent to the pricing page.
	if err := s.plans.Check(ctx, tenantID, domain.ResourceRestaurants); err != nil {
		return domain.Restaurant{}, err
	}
	if err := s.plans.CheckValue(ctx, tenantID, domain.ResourceLocales, len(in.Locales)); err != nil {
		return domain.Restaurant{}, err
	}

	nameRaw, err := in.Name.Raw()
	if err != nil {
		return domain.Restaurant{}, err
	}
	descRaw, err := in.Description.Raw()
	if err != nil {
		return domain.Restaurant{}, err
	}

	row, err := s.store.CreateRestaurant(ctx, gen.CreateRestaurantParams{
		TenantID:      tenantID,
		Slug:          in.Slug,
		Name:          nameRaw,
		Description:   descRaw,
		Currency:      in.Currency,
		Timezone:      in.Timezone,
		DefaultLocale: in.DefaultLocale,
		Locales:       in.Locales,
		Phone:         in.Phone,
		Address:       in.Address,
	})
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("create restaurant: %w", postgres.MapError(err))
	}
	return postgres.ToRestaurant(row)
}

func (s *Restaurant) Get(ctx context.Context, tenantID, id uuid.UUID) (domain.Restaurant, error) {
	row, err := s.store.GetRestaurant(ctx, gen.GetRestaurantParams{ID: id, TenantID: tenantID})
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("get restaurant: %w", postgres.MapError(err))
	}
	return postgres.ToRestaurant(row)
}

func (s *Restaurant) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Restaurant, error) {
	rows, err := s.store.ListRestaurants(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list restaurants: %w", postgres.MapError(err))
	}
	return postgres.ToRestaurants(rows)
}

// RestaurantPatch carries only the fields the client actually sent. A nil
// pointer means "leave this alone", which is what makes PATCH different from
// PUT: omitting a field must not blank it.
type RestaurantPatch struct {
	Slug          *string
	Name          *i18n.Text
	Description   *i18n.Text
	Currency      *string
	Timezone      *string
	DefaultLocale *string
	Locales       *[]string
	Phone         *string
	Address       *string
	Theme         *map[string]any
	IsActive      *bool

	// The logo and cover are set by attaching an already-uploaded image. As
	// with items, the composite foreign key is what enforces that the image
	// belongs to this restaurant.
	LogoImageID  *uuid.UUID
	ClearLogo    bool
	CoverImageID *uuid.UUID
	ClearCover   bool
}

// Update applies a partial change.
//
// The patch is merged onto the current row and the *result* is validated, not
// the patch alone. Otherwise changing only default_locale could point it at a
// language that locales does not contain — each field looks fine on its own and
// the combination is broken.
func (s *Restaurant) Update(ctx context.Context, tenantID, id uuid.UUID, patch RestaurantPatch) (domain.Restaurant, error) {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Restaurant{}, err
	}

	merged := domain.RestaurantInput{
		Slug:          current.Slug,
		Name:          current.Name,
		Description:   current.Description,
		Currency:      current.Currency,
		Timezone:      current.Timezone,
		DefaultLocale: current.DefaultLocale,
		Locales:       current.Locales,
		Phone:         current.Phone,
		Address:       current.Address,
	}
	if patch.Slug != nil {
		merged.Slug = *patch.Slug
	}
	if patch.Name != nil {
		merged.Name = *patch.Name
	}
	if patch.Description != nil {
		merged.Description = *patch.Description
	}
	if patch.Currency != nil {
		merged.Currency = *patch.Currency
	}
	if patch.Timezone != nil {
		merged.Timezone = *patch.Timezone
	}
	if patch.DefaultLocale != nil {
		merged.DefaultLocale = *patch.DefaultLocale
	}
	if patch.Locales != nil {
		merged.Locales = *patch.Locales
	}
	if patch.Phone != nil {
		merged.Phone = patch.Phone
	}
	if patch.Address != nil {
		merged.Address = patch.Address
	}

	merged = normalizeRestaurantInput(merged)
	if err := merged.Validate(); err != nil {
		return domain.Restaurant{}, err
	}

	// A PATCH replaces the locales array wholesale, so the question is whether
	// the result is allowed, not whether one more may be added. Checked against
	// the merged value for the same reason validation is: the patch alone does
	// not say what the restaurant will end up with.
	if patch.Locales != nil {
		if err := s.plans.CheckValue(ctx, tenantID, domain.ResourceLocales, len(merged.Locales)); err != nil {
			return domain.Restaurant{}, err
		}
	}

	params := gen.UpdateRestaurantParams{ID: id, TenantID: tenantID}

	if patch.Slug != nil {
		params.Slug = &merged.Slug
	}
	if patch.Name != nil {
		raw, err := merged.Name.Raw()
		if err != nil {
			return domain.Restaurant{}, err
		}
		params.Name = raw
	}
	if patch.Description != nil {
		raw, err := merged.Description.Raw()
		if err != nil {
			return domain.Restaurant{}, err
		}
		params.Description = raw
	}
	if patch.Currency != nil {
		params.Currency = &merged.Currency
	}
	if patch.Timezone != nil {
		params.Timezone = &merged.Timezone
	}
	if patch.DefaultLocale != nil {
		params.DefaultLocale = &merged.DefaultLocale
	}
	if patch.Locales != nil {
		params.Locales = merged.Locales
	}
	if patch.Phone != nil {
		params.Phone = merged.Phone
	}
	if patch.Address != nil {
		params.Address = merged.Address
	}
	if patch.Theme != nil {
		raw, err := json.Marshal(*patch.Theme)
		if err != nil {
			return domain.Restaurant{}, fmt.Errorf("encode theme: %w", err)
		}
		params.Theme = raw
	}
	if patch.IsActive != nil {
		params.IsActive = patch.IsActive
	}

	params.ClearLogo = patch.ClearLogo
	if !patch.ClearLogo {
		params.LogoImageID = patch.LogoImageID
	}
	params.ClearCover = patch.ClearCover
	if !patch.ClearCover {
		params.CoverImageID = patch.CoverImageID
	}

	row, err := s.store.UpdateRestaurant(ctx, params)
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("update restaurant: %w", postgres.MapError(err))
	}
	return postgres.ToRestaurant(row)
}

func (s *Restaurant) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	n, err := s.store.DeleteRestaurant(ctx, gen.DeleteRestaurantParams{ID: id, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("delete restaurant: %w", postgres.MapError(err))
	}
	// Zero rows means the restaurant either does not exist or belongs to
	// another tenant. Both must look identical from outside, or the API becomes
	// a way to probe which IDs are real.
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func normalizeRestaurantInput(in domain.RestaurantInput) domain.RestaurantInput {
	in.Slug = domain.NormalizeSlug(in.Slug)
	in.Name = in.Name.Normalize()
	in.Description = in.Description.Normalize()
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Timezone = strings.TrimSpace(in.Timezone)
	in.DefaultLocale = strings.ToLower(strings.TrimSpace(in.DefaultLocale))

	locales := make([]string, 0, len(in.Locales))
	for _, l := range in.Locales {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			locales = append(locales, l)
		}
	}
	in.Locales = locales

	in.Phone = trimmedPtr(in.Phone)
	in.Address = trimmedPtr(in.Address)
	return in
}

func trimmedPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}
