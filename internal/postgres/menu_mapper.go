package postgres

import (
	"fmt"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// translated decodes the two jsonb fields every menu entity carries. Pulling it
// out keeps six near-identical mappers from repeating the same error handling.
func translated(entity string, id fmt.Stringer, nameRaw, descRaw []byte) (i18n.Text, i18n.Text, error) {
	name, err := i18n.FromRaw(nameRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s name: %w", entity, id, err)
	}
	description, err := i18n.FromRaw(descRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s description: %w", entity, id, err)
	}
	return name, description, nil
}

func ToMenu(m gen.Menu) (domain.Menu, error) {
	name, description, err := translated("menu", m.ID, m.Name, m.Description)
	if err != nil {
		return domain.Menu{}, err
	}
	return domain.Menu{
		ID:               m.ID,
		RestaurantID:     m.RestaurantID,
		Name:             name,
		Description:      description,
		IsActive:         m.IsActive,
		SortOrder:        m.SortOrder,
		AvailableFromMin: m.AvailableFromMin,
		AvailableToMin:   m.AvailableToMin,
		Days:             m.Days,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}, nil
}

func ToMenus(rows []gen.Menu) ([]domain.Menu, error) {
	return mapRows(rows, ToMenu)
}

func ToCategory(c gen.Category) (domain.Category, error) {
	name, description, err := translated("category", c.ID, c.Name, c.Description)
	if err != nil {
		return domain.Category{}, err
	}
	return domain.Category{
		ID:           c.ID,
		RestaurantID: c.RestaurantID,
		MenuID:       c.MenuID,
		Name:         name,
		Description:  description,
		IsActive:     c.IsActive,
		SortOrder:    c.SortOrder,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}, nil
}

func ToCategories(rows []gen.Category) ([]domain.Category, error) {
	return mapRows(rows, ToCategory)
}

func ToItem(i gen.Item) (domain.Item, error) {
	name, description, err := translated("item", i.ID, i.Name, i.Description)
	if err != nil {
		return domain.Item{}, err
	}
	return domain.Item{
		ID:           i.ID,
		RestaurantID: i.RestaurantID,
		CategoryID:   i.CategoryID,
		Name:         name,
		Description:  description,
		PriceMinor:   i.PriceMinor,
		IsAvailable:  i.IsAvailable,
		SortOrder:    i.SortOrder,
		Tags:         i.Tags,
		Allergens:    i.Allergens,
		Calories:     i.Calories,
		PrepMinutes:  i.PrepMinutes,
		ImageID:      i.ImageID,
		CreatedAt:    i.CreatedAt,
		UpdatedAt:    i.UpdatedAt,
	}, nil
}

func ToItems(rows []gen.Item) ([]domain.Item, error) {
	return mapRows(rows, ToItem)
}

func ToVariant(v gen.ItemVariant) (domain.Variant, error) {
	name, err := i18n.FromRaw(v.Name)
	if err != nil {
		return domain.Variant{}, fmt.Errorf("variant %s name: %w", v.ID, err)
	}
	return domain.Variant{
		ID:              v.ID,
		RestaurantID:    v.RestaurantID,
		ItemID:          v.ItemID,
		Name:            name,
		PriceDeltaMinor: v.PriceDeltaMinor,
		IsDefault:       v.IsDefault,
		IsAvailable:     v.IsAvailable,
		SortOrder:       v.SortOrder,
		CreatedAt:       v.CreatedAt,
		UpdatedAt:       v.UpdatedAt,
	}, nil
}

func ToVariants(rows []gen.ItemVariant) ([]domain.Variant, error) {
	return mapRows(rows, ToVariant)
}

func ToModifierGroup(g gen.ModifierGroup) (domain.ModifierGroup, error) {
	name, err := i18n.FromRaw(g.Name)
	if err != nil {
		return domain.ModifierGroup{}, fmt.Errorf("modifier group %s name: %w", g.ID, err)
	}
	return domain.ModifierGroup{
		ID:           g.ID,
		RestaurantID: g.RestaurantID,
		ItemID:       g.ItemID,
		Name:         name,
		MinSelect:    g.MinSelect,
		MaxSelect:    g.MaxSelect,
		SortOrder:    g.SortOrder,
		CreatedAt:    g.CreatedAt,
		UpdatedAt:    g.UpdatedAt,
	}, nil
}

func ToModifierGroups(rows []gen.ModifierGroup) ([]domain.ModifierGroup, error) {
	return mapRows(rows, ToModifierGroup)
}

func ToModifier(m gen.Modifier) (domain.Modifier, error) {
	name, err := i18n.FromRaw(m.Name)
	if err != nil {
		return domain.Modifier{}, fmt.Errorf("modifier %s name: %w", m.ID, err)
	}
	return domain.Modifier{
		ID:              m.ID,
		RestaurantID:    m.RestaurantID,
		GroupID:         m.GroupID,
		Name:            name,
		PriceDeltaMinor: m.PriceDeltaMinor,
		IsAvailable:     m.IsAvailable,
		SortOrder:       m.SortOrder,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
	}, nil
}

func ToModifiers(rows []gen.Modifier) ([]domain.Modifier, error) {
	return mapRows(rows, ToModifier)
}

// mapRows applies a fallible mapper across a slice, returning an empty (not
// nil) slice for no rows so JSON encodes [] rather than null.
func mapRows[R any, D any](rows []R, mapper func(R) (D, error)) ([]D, error) {
	out := make([]D, len(rows))
	for i, r := range rows {
		mapped, err := mapper(r)
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	return out, nil
}
