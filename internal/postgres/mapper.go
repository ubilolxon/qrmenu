package postgres

import (
	"encoding/json"
	"fmt"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// Mapping generated row structs to domain types is what keeps the database
// shape from leaking upward. It is also the only place a column rename has to
// be reconciled, since sqlc will have already failed the build everywhere else.

func ToTenant(t gen.Tenant) domain.Tenant {
	return domain.Tenant{
		ID:        t.ID,
		Name:      t.Name,
		Status:    domain.TenantStatus(t.Status),
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

// ToUser drops PasswordHash: the domain type has no field for it, so a hash
// cannot be serialised into a response by accident.
func ToUser(u gen.User) domain.User {
	return domain.User{
		ID:          u.ID,
		TenantID:    u.TenantID,
		Email:       u.Email,
		Name:        u.Name,
		Role:        domain.Role(u.Role),
		IsActive:    u.IsActive,
		LastLoginAt: u.LastLoginAt,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

func ToUsers(rows []gen.User) []domain.User {
	out := make([]domain.User, len(rows))
	for i, r := range rows {
		out[i] = ToUser(r)
	}
	return out
}

func ToRestaurant(r gen.Restaurant) (domain.Restaurant, error) {
	name, err := i18n.FromRaw(r.Name)
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("restaurant %s name: %w", r.ID, err)
	}
	description, err := i18n.FromRaw(r.Description)
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("restaurant %s description: %w", r.ID, err)
	}

	theme := map[string]any{}
	if len(r.Theme) > 0 && string(r.Theme) != "null" {
		if err := json.Unmarshal(r.Theme, &theme); err != nil {
			return domain.Restaurant{}, fmt.Errorf("restaurant %s theme: %w", r.ID, err)
		}
	}

	return domain.Restaurant{
		ID:            r.ID,
		TenantID:      r.TenantID,
		Slug:          r.Slug,
		Name:          name,
		Description:   description,
		Currency:      r.Currency,
		Timezone:      r.Timezone,
		DefaultLocale: r.DefaultLocale,
		Locales:       r.Locales,
		Phone:         r.Phone,
		Address:       r.Address,
		Theme:         theme,
		LogoImageID:   r.LogoImageID,
		CoverImageID:  r.CoverImageID,
		MenuVersion:   r.MenuVersion,
		IsActive:      r.IsActive,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}, nil
}

func ToRestaurants(rows []gen.Restaurant) ([]domain.Restaurant, error) {
	out := make([]domain.Restaurant, len(rows))
	for i, r := range rows {
		mapped, err := ToRestaurant(r)
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	return out, nil
}

func ToTable(t gen.RestaurantTable) domain.Table {
	return domain.Table{
		ID:           t.ID,
		RestaurantID: t.RestaurantID,
		Label:        t.Label,
		QRToken:      t.QrToken,
		Seats:        t.Seats,
		SortOrder:    t.SortOrder,
		IsActive:     t.IsActive,
		CreatedAt:    t.CreatedAt,
		UpdatedAt:    t.UpdatedAt,
	}
}

func ToTables(rows []gen.RestaurantTable) []domain.Table {
	out := make([]domain.Table, len(rows))
	for i, r := range rows {
		out[i] = ToTable(r)
	}
	return out
}

// ToImage decodes the variants JSON as it maps.
//
// A row whose variants column will not parse is returned with an empty map
// rather than an error: the image is then missing sizes, and domain.ToPublic
// refuses to publish it, which is the right outcome. Failing the whole menu
// read because one photo's metadata is corrupt is not.
func ToImage(img gen.Image) domain.Image {
	variants := map[string]domain.ImageVariant{}
	if len(img.Variants) > 0 {
		if err := json.Unmarshal(img.Variants, &variants); err != nil {
			variants = map[string]domain.ImageVariant{}
		}
	}

	return domain.Image{
		ID:           img.ID,
		RestaurantID: img.RestaurantID,
		Status:       domain.ImageStatus(img.Status),
		StorageKey:   img.StorageKey,
		ContentType:  img.ContentType,
		ByteSize:     img.ByteSize,
		Width:        img.Width,
		Height:       img.Height,
		Placeholder:  img.Placeholder,
		Variants:     variants,
		Error:        img.Error,
		CreatedAt:    img.CreatedAt,
		UpdatedAt:    img.UpdatedAt,
		UploadedAt:   img.UploadedAt,
	}
}

func ToImages(rows []gen.Image) []domain.Image {
	out := make([]domain.Image, len(rows))
	for i, r := range rows {
		out[i] = ToImage(r)
	}
	return out
}
