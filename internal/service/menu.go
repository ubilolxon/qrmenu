package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// Menu owns the whole menu tree: menus, categories, items, variants and
// modifiers.
//
// Tenant scoping works in two steps here, because the child tables carry a
// restaurant rather than a tenant:
//
//  1. assertRestaurant proves the restaurant belongs to the caller's tenant.
//  2. Every child query filters on restaurant_id.
//
// Both are mandatory. Step 1 alone would let a caller touch a sibling
// restaurant's rows; step 2 alone would let any tenant name any restaurant.
type Menu struct {
	store *postgres.Store
}

func NewMenu(store *postgres.Store) *Menu {
	return &Menu{store: store}
}

// assertRestaurant is the tenant boundary for everything in this file.
func (s *Menu) assertRestaurant(ctx context.Context, tenantID, restaurantID uuid.UUID) error {
	_, err := s.store.GetRestaurant(ctx, gen.GetRestaurantParams{ID: restaurantID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("restaurant: %w", postgres.MapError(err))
	}
	return nil
}

// write runs a mutation and bumps the restaurant's menu_version in the same
// transaction.
//
// The version is the public cache key suffix (Phase 3). Bumping it separately
// would leave a window where the menu has changed but the cache key has not,
// and guests would be served a stale menu with no way to notice. Committing
// both together makes that impossible.
func (s *Menu) write(ctx context.Context, tenantID, restaurantID uuid.UUID, fn func(*gen.Queries) error) error {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return err
	}
	return s.store.InTx(ctx, func(q *gen.Queries) error {
		if err := fn(q); err != nil {
			return err
		}
		if _, err := q.BumpMenuVersion(ctx, gen.BumpMenuVersionParams{
			ID:       restaurantID,
			TenantID: tenantID,
		}); err != nil {
			return fmt.Errorf("bump menu version: %w", postgres.MapError(err))
		}
		return nil
	})
}

// read verifies ownership before a query that only filters by restaurant.
func (s *Menu) read(ctx context.Context, tenantID, restaurantID uuid.UUID) error {
	return s.assertRestaurant(ctx, tenantID, restaurantID)
}

// ---------------------------------------------------------------- menus

func (s *Menu) CreateMenu(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.MenuInput) (domain.Menu, error) {
	in.Name = in.Name.Normalize()
	in.Description = in.Description.Normalize()
	// The column is NOT NULL with a '{}' default, but naming it in the INSERT
	// means the default never applies — a nil slice would be sent as NULL.
	if in.Days == nil {
		in.Days = []int16{}
	}
	if err := in.Validate(); err != nil {
		return domain.Menu{}, err
	}

	nameRaw, descRaw, err := rawPair(in.Name, in.Description)
	if err != nil {
		return domain.Menu{}, err
	}

	var row gen.Menu
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.CreateMenu(ctx, gen.CreateMenuParams{
			RestaurantID:     restaurantID,
			Name:             nameRaw,
			Description:      descRaw,
			SortOrder:        in.SortOrder,
			AvailableFromMin: in.AvailableFromMin,
			AvailableToMin:   in.AvailableToMin,
			Days:             in.Days,
		})
		return wrap("create menu", err)
	})
	if err != nil {
		return domain.Menu{}, err
	}
	return postgres.ToMenu(row)
}

func (s *Menu) GetMenu(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Menu, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return domain.Menu{}, err
	}
	row, err := s.store.GetMenu(ctx, gen.GetMenuParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Menu{}, wrap("get menu", err)
	}
	return postgres.ToMenu(row)
}

func (s *Menu) ListMenus(ctx context.Context, tenantID, restaurantID uuid.UUID) ([]domain.Menu, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListMenus(ctx, restaurantID)
	if err != nil {
		return nil, wrap("list menus", err)
	}
	return postgres.ToMenus(rows)
}

// MenuPatch carries only the fields the client sent. ClearWindow is separate
// from the two window fields because a nil pointer already means "leave alone",
// so there would otherwise be no way to express "remove the window".
type MenuPatch struct {
	Name             *i18n.Text
	Description      *i18n.Text
	IsActive         *bool
	SortOrder        *int32
	AvailableFromMin *int32
	AvailableToMin   *int32
	ClearWindow      bool
	Days             *[]int16
}

func (s *Menu) UpdateMenu(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch MenuPatch) (domain.Menu, error) {
	current, err := s.GetMenu(ctx, tenantID, restaurantID, id)
	if err != nil {
		return domain.Menu{}, err
	}

	// Validate the merged result, not the patch: a window endpoint and a day
	// list are individually fine but can be contradictory together.
	merged := domain.MenuInput{
		Name:             current.Name,
		Description:      current.Description,
		SortOrder:        current.SortOrder,
		AvailableFromMin: current.AvailableFromMin,
		AvailableToMin:   current.AvailableToMin,
		Days:             current.Days,
	}
	if patch.Name != nil {
		merged.Name = patch.Name.Normalize()
	}
	if patch.Description != nil {
		merged.Description = patch.Description.Normalize()
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if patch.Days != nil {
		merged.Days = *patch.Days
	}
	if patch.ClearWindow {
		merged.AvailableFromMin, merged.AvailableToMin = nil, nil
	} else {
		if patch.AvailableFromMin != nil {
			merged.AvailableFromMin = patch.AvailableFromMin
		}
		if patch.AvailableToMin != nil {
			merged.AvailableToMin = patch.AvailableToMin
		}
	}
	if err := merged.Validate(); err != nil {
		return domain.Menu{}, err
	}

	params := gen.UpdateMenuParams{
		ID:           id,
		RestaurantID: restaurantID,
		IsActive:     patch.IsActive,
		SortOrder:    patch.SortOrder,
		ClearWindow:  patch.ClearWindow,
	}
	if patch.Name != nil {
		if params.Name, err = merged.Name.Raw(); err != nil {
			return domain.Menu{}, err
		}
	}
	if patch.Description != nil {
		if params.Description, err = merged.Description.Raw(); err != nil {
			return domain.Menu{}, err
		}
	}
	if patch.Days != nil {
		params.Days = merged.Days
	}
	if !patch.ClearWindow {
		params.AvailableFromMin = patch.AvailableFromMin
		params.AvailableToMin = patch.AvailableToMin
	}

	var row gen.Menu
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.UpdateMenu(ctx, params)
		return wrap("update menu", err)
	})
	if err != nil {
		return domain.Menu{}, err
	}
	return postgres.ToMenu(row)
}

// ReorderMenus sets sort_order from the position of each id in the list.
func (s *Menu) ReorderMenus(ctx context.Context, tenantID, restaurantID uuid.UUID, ids []uuid.UUID) error {
	if err := domain.ValidateReorder(ids); err != nil {
		return err
	}
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.ReorderMenus(ctx, gen.ReorderMenusParams{Ids: ids, RestaurantID: restaurantID})
		if err != nil {
			return wrap("reorder menus", err)
		}
		// Ids belonging to another restaurant match nothing, so a short count
		// means the caller sent something that is not theirs. Rejecting the
		// whole batch keeps the reorder all-or-nothing.
		if int(n) != len(ids) {
			return domain.Errorf(domain.ErrNotFound, "Some of those menus do not exist.")
		}
		return nil
	})
}

func (s *Menu) DeleteMenu(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.DeleteMenu(ctx, gen.DeleteMenuParams{ID: id, RestaurantID: restaurantID})
		if err != nil {
			return wrap("delete menu", err)
		}
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- categories

func (s *Menu) CreateCategory(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.CategoryInput) (domain.Category, error) {
	in.Name = in.Name.Normalize()
	in.Description = in.Description.Normalize()
	if err := in.Validate(); err != nil {
		return domain.Category{}, err
	}

	nameRaw, descRaw, err := rawPair(in.Name, in.Description)
	if err != nil {
		return domain.Category{}, err
	}

	var row gen.Category
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.CreateCategory(ctx, gen.CreateCategoryParams{
			RestaurantID: restaurantID,
			MenuID:       in.MenuID,
			Name:         nameRaw,
			Description:  descRaw,
			SortOrder:    in.SortOrder,
		})
		// The composite foreign key rejects a menu from another restaurant, so
		// this surfaces as a not-found rather than a leak.
		return wrap("create category", err)
	})
	if err != nil {
		return domain.Category{}, err
	}
	return postgres.ToCategory(row)
}

func (s *Menu) GetCategory(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Category, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return domain.Category{}, err
	}
	row, err := s.store.GetCategory(ctx, gen.GetCategoryParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Category{}, wrap("get category", err)
	}
	return postgres.ToCategory(row)
}

func (s *Menu) ListCategories(ctx context.Context, tenantID, restaurantID, menuID uuid.UUID) ([]domain.Category, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListCategories(ctx, gen.ListCategoriesParams{MenuID: menuID, RestaurantID: restaurantID})
	if err != nil {
		return nil, wrap("list categories", err)
	}
	return postgres.ToCategories(rows)
}

type CategoryPatch struct {
	MenuID      *uuid.UUID
	Name        *i18n.Text
	Description *i18n.Text
	IsActive    *bool
	SortOrder   *int32
}

func (s *Menu) UpdateCategory(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch CategoryPatch) (domain.Category, error) {
	current, err := s.GetCategory(ctx, tenantID, restaurantID, id)
	if err != nil {
		return domain.Category{}, err
	}

	merged := domain.CategoryInput{
		MenuID:      current.MenuID,
		Name:        current.Name,
		Description: current.Description,
		SortOrder:   current.SortOrder,
	}
	if patch.MenuID != nil {
		merged.MenuID = *patch.MenuID
	}
	if patch.Name != nil {
		merged.Name = patch.Name.Normalize()
	}
	if patch.Description != nil {
		merged.Description = patch.Description.Normalize()
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if err := merged.Validate(); err != nil {
		return domain.Category{}, err
	}

	params := gen.UpdateCategoryParams{
		ID:           id,
		RestaurantID: restaurantID,
		IsActive:     patch.IsActive,
		SortOrder:    patch.SortOrder,
		MenuID:       patch.MenuID,
	}
	if patch.Name != nil {
		if params.Name, err = merged.Name.Raw(); err != nil {
			return domain.Category{}, err
		}
	}
	if patch.Description != nil {
		if params.Description, err = merged.Description.Raw(); err != nil {
			return domain.Category{}, err
		}
	}

	var row gen.Category
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.UpdateCategory(ctx, params)
		return wrap("update category", err)
	})
	if err != nil {
		return domain.Category{}, err
	}
	return postgres.ToCategory(row)
}

func (s *Menu) ReorderCategories(ctx context.Context, tenantID, restaurantID uuid.UUID, ids []uuid.UUID) error {
	if err := domain.ValidateReorder(ids); err != nil {
		return err
	}
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.ReorderCategories(ctx, gen.ReorderCategoriesParams{Ids: ids, RestaurantID: restaurantID})
		if err != nil {
			return wrap("reorder categories", err)
		}
		if int(n) != len(ids) {
			return domain.Errorf(domain.ErrNotFound, "Some of those categories do not exist.")
		}
		return nil
	})
}

func (s *Menu) DeleteCategory(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.DeleteCategory(ctx, gen.DeleteCategoryParams{ID: id, RestaurantID: restaurantID})
		if err != nil {
			return wrap("delete category", err)
		}
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- items

func (s *Menu) CreateItem(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.ItemInput) (domain.Item, error) {
	in.Name = in.Name.Normalize()
	in.Description = in.Description.Normalize()
	in.Tags = domain.NormalizeTags(in.Tags)
	in.Allergens = domain.NormalizeTags(in.Allergens)
	if err := in.Validate(); err != nil {
		return domain.Item{}, err
	}

	nameRaw, descRaw, err := rawPair(in.Name, in.Description)
	if err != nil {
		return domain.Item{}, err
	}

	var row gen.Item
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.CreateItem(ctx, gen.CreateItemParams{
			RestaurantID: restaurantID,
			CategoryID:   in.CategoryID,
			Name:         nameRaw,
			Description:  descRaw,
			PriceMinor:   in.PriceMinor,
			SortOrder:    in.SortOrder,
			Tags:         in.Tags,
			Allergens:    in.Allergens,
			Calories:     in.Calories,
			PrepMinutes:  in.PrepMinutes,
		})
		return wrap("create item", err)
	})
	if err != nil {
		return domain.Item{}, err
	}
	return postgres.ToItem(row)
}

func (s *Menu) GetItem(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Item, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return domain.Item{}, err
	}
	row, err := s.store.GetItem(ctx, gen.GetItemParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Item{}, wrap("get item", err)
	}
	return postgres.ToItem(row)
}

func (s *Menu) ListItems(ctx context.Context, tenantID, restaurantID, categoryID uuid.UUID) ([]domain.Item, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListItems(ctx, gen.ListItemsParams{CategoryID: categoryID, RestaurantID: restaurantID})
	if err != nil {
		return nil, wrap("list items", err)
	}
	return postgres.ToItems(rows)
}

type ItemPatch struct {
	CategoryID       *uuid.UUID
	Name             *i18n.Text
	Description      *i18n.Text
	PriceMinor       *int64
	IsAvailable      *bool
	SortOrder        *int32
	Tags             *[]string
	Allergens        *[]string
	Calories         *int32
	ClearCalories    bool
	PrepMinutes      *int32
	ClearPrepMinutes bool
	ImageID          *uuid.UUID
	ClearImage       bool
}

func (s *Menu) UpdateItem(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch ItemPatch) (domain.Item, error) {
	current, err := s.GetItem(ctx, tenantID, restaurantID, id)
	if err != nil {
		return domain.Item{}, err
	}

	merged := domain.ItemInput{
		CategoryID:  current.CategoryID,
		Name:        current.Name,
		Description: current.Description,
		PriceMinor:  current.PriceMinor,
		SortOrder:   current.SortOrder,
		Tags:        current.Tags,
		Allergens:   current.Allergens,
		Calories:    current.Calories,
		PrepMinutes: current.PrepMinutes,
	}
	if patch.CategoryID != nil {
		merged.CategoryID = *patch.CategoryID
	}
	if patch.Name != nil {
		merged.Name = patch.Name.Normalize()
	}
	if patch.Description != nil {
		merged.Description = patch.Description.Normalize()
	}
	if patch.PriceMinor != nil {
		merged.PriceMinor = *patch.PriceMinor
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if patch.Tags != nil {
		merged.Tags = domain.NormalizeTags(*patch.Tags)
	}
	if patch.Allergens != nil {
		merged.Allergens = domain.NormalizeTags(*patch.Allergens)
	}
	if patch.ClearCalories {
		merged.Calories = nil
	} else if patch.Calories != nil {
		merged.Calories = patch.Calories
	}
	if patch.ClearPrepMinutes {
		merged.PrepMinutes = nil
	} else if patch.PrepMinutes != nil {
		merged.PrepMinutes = patch.PrepMinutes
	}
	if err := merged.Validate(); err != nil {
		return domain.Item{}, err
	}

	params := gen.UpdateItemParams{
		ID:               id,
		RestaurantID:     restaurantID,
		PriceMinor:       patch.PriceMinor,
		IsAvailable:      patch.IsAvailable,
		SortOrder:        patch.SortOrder,
		CategoryID:       patch.CategoryID,
		ClearCalories:    patch.ClearCalories,
		ClearPrepMinutes: patch.ClearPrepMinutes,
		ClearImage:       patch.ClearImage,
	}
	if patch.Name != nil {
		if params.Name, err = merged.Name.Raw(); err != nil {
			return domain.Item{}, err
		}
	}
	if patch.Description != nil {
		if params.Description, err = merged.Description.Raw(); err != nil {
			return domain.Item{}, err
		}
	}
	if patch.Tags != nil {
		params.Tags = merged.Tags
	}
	if patch.Allergens != nil {
		params.Allergens = merged.Allergens
	}
	if !patch.ClearCalories {
		params.Calories = patch.Calories
	}
	if !patch.ClearPrepMinutes {
		params.PrepMinutes = patch.PrepMinutes
	}
	// No ownership check on the image id: the composite foreign key on
	// (image_id, restaurant_id) makes attaching another restaurant's photo a
	// constraint violation, which MapError renders as "referenced record does
	// not exist".
	if !patch.ClearImage {
		params.ImageID = patch.ImageID
	}

	var row gen.Item
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.UpdateItem(ctx, params)
		return wrap("update item", err)
	})
	if err != nil {
		return domain.Item{}, err
	}
	return postgres.ToItem(row)
}

// SetItemAvailability is the "we ran out of plov" button. It is separate from
// UpdateItem because it is the one menu write that happens mid-service, and it
// should not require sending or re-validating the whole item.
func (s *Menu) SetItemAvailability(ctx context.Context, tenantID, restaurantID, id uuid.UUID, available bool) (domain.Item, error) {
	var row gen.Item
	err := s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.SetItemAvailability(ctx, gen.SetItemAvailabilityParams{
			ID:           id,
			RestaurantID: restaurantID,
			IsAvailable:  available,
		})
		return wrap("set item availability", err)
	})
	if err != nil {
		return domain.Item{}, err
	}
	return postgres.ToItem(row)
}

func (s *Menu) ReorderItems(ctx context.Context, tenantID, restaurantID uuid.UUID, ids []uuid.UUID) error {
	if err := domain.ValidateReorder(ids); err != nil {
		return err
	}
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.ReorderItems(ctx, gen.ReorderItemsParams{Ids: ids, RestaurantID: restaurantID})
		if err != nil {
			return wrap("reorder items", err)
		}
		if int(n) != len(ids) {
			return domain.Errorf(domain.ErrNotFound, "Some of those items do not exist.")
		}
		return nil
	})
}

func (s *Menu) DeleteItem(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.DeleteItem(ctx, gen.DeleteItemParams{ID: id, RestaurantID: restaurantID})
		if err != nil {
			return wrap("delete item", err)
		}
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- variants

func (s *Menu) CreateVariant(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.VariantInput) (domain.Variant, error) {
	in.Name = in.Name.Normalize()
	if err := in.Validate(); err != nil {
		return domain.Variant{}, err
	}

	nameRaw, err := in.Name.Raw()
	if err != nil {
		return domain.Variant{}, err
	}

	var row gen.ItemVariant
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		// A partial unique index allows one default per item, so the previous
		// default has to go first — in this same transaction, or a concurrent
		// reader could briefly see an item with two defaults or none.
		if in.IsDefault {
			if _, err := q.ClearDefaultVariant(ctx, gen.ClearDefaultVariantParams{
				ItemID: in.ItemID, RestaurantID: restaurantID,
			}); err != nil {
				return wrap("clear default variant", err)
			}
		}
		var err error
		row, err = q.CreateVariant(ctx, gen.CreateVariantParams{
			RestaurantID:    restaurantID,
			ItemID:          in.ItemID,
			Name:            nameRaw,
			PriceDeltaMinor: in.PriceDeltaMinor,
			IsDefault:       in.IsDefault,
			SortOrder:       in.SortOrder,
		})
		return wrap("create variant", err)
	})
	if err != nil {
		return domain.Variant{}, err
	}
	return postgres.ToVariant(row)
}

func (s *Menu) ListVariants(ctx context.Context, tenantID, restaurantID, itemID uuid.UUID) ([]domain.Variant, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListVariants(ctx, gen.ListVariantsParams{ItemID: itemID, RestaurantID: restaurantID})
	if err != nil {
		return nil, wrap("list variants", err)
	}
	return postgres.ToVariants(rows)
}

type VariantPatch struct {
	Name            *i18n.Text
	PriceDeltaMinor *int64
	IsDefault       *bool
	IsAvailable     *bool
	SortOrder       *int32
}

func (s *Menu) UpdateVariant(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch VariantPatch) (domain.Variant, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return domain.Variant{}, err
	}
	current, err := s.store.GetVariant(ctx, gen.GetVariantParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Variant{}, wrap("get variant", err)
	}

	merged := domain.VariantInput{
		ItemID:          current.ItemID,
		Name:            mustText(current.Name),
		PriceDeltaMinor: current.PriceDeltaMinor,
		SortOrder:       current.SortOrder,
	}
	if patch.Name != nil {
		merged.Name = patch.Name.Normalize()
	}
	if patch.PriceDeltaMinor != nil {
		merged.PriceDeltaMinor = *patch.PriceDeltaMinor
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if err := merged.Validate(); err != nil {
		return domain.Variant{}, err
	}

	params := gen.UpdateVariantParams{
		ID:              id,
		RestaurantID:    restaurantID,
		PriceDeltaMinor: patch.PriceDeltaMinor,
		IsDefault:       patch.IsDefault,
		IsAvailable:     patch.IsAvailable,
		SortOrder:       patch.SortOrder,
	}
	if patch.Name != nil {
		if params.Name, err = merged.Name.Raw(); err != nil {
			return domain.Variant{}, err
		}
	}

	var row gen.ItemVariant
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		if patch.IsDefault != nil && *patch.IsDefault {
			if _, err := q.ClearDefaultVariant(ctx, gen.ClearDefaultVariantParams{
				ItemID: current.ItemID, RestaurantID: restaurantID,
			}); err != nil {
				return wrap("clear default variant", err)
			}
		}
		var err error
		row, err = q.UpdateVariant(ctx, params)
		return wrap("update variant", err)
	})
	if err != nil {
		return domain.Variant{}, err
	}
	return postgres.ToVariant(row)
}

func (s *Menu) DeleteVariant(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.DeleteVariant(ctx, gen.DeleteVariantParams{ID: id, RestaurantID: restaurantID})
		if err != nil {
			return wrap("delete variant", err)
		}
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- modifiers

func (s *Menu) CreateModifierGroup(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.ModifierGroupInput) (domain.ModifierGroup, error) {
	in.Name = in.Name.Normalize()
	if err := in.Validate(); err != nil {
		return domain.ModifierGroup{}, err
	}

	nameRaw, err := in.Name.Raw()
	if err != nil {
		return domain.ModifierGroup{}, err
	}

	var row gen.ModifierGroup
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.CreateModifierGroup(ctx, gen.CreateModifierGroupParams{
			RestaurantID: restaurantID,
			ItemID:       in.ItemID,
			Name:         nameRaw,
			MinSelect:    in.MinSelect,
			MaxSelect:    in.MaxSelect,
			SortOrder:    in.SortOrder,
		})
		return wrap("create modifier group", err)
	})
	if err != nil {
		return domain.ModifierGroup{}, err
	}
	return postgres.ToModifierGroup(row)
}

func (s *Menu) ListModifierGroups(ctx context.Context, tenantID, restaurantID, itemID uuid.UUID) ([]domain.ModifierGroup, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListModifierGroups(ctx, gen.ListModifierGroupsParams{ItemID: itemID, RestaurantID: restaurantID})
	if err != nil {
		return nil, wrap("list modifier groups", err)
	}
	return postgres.ToModifierGroups(rows)
}

type ModifierGroupPatch struct {
	Name      *i18n.Text
	MinSelect *int32
	MaxSelect *int32
	SortOrder *int32
}

func (s *Menu) UpdateModifierGroup(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch ModifierGroupPatch) (domain.ModifierGroup, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return domain.ModifierGroup{}, err
	}
	current, err := s.store.GetModifierGroup(ctx, gen.GetModifierGroupParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.ModifierGroup{}, wrap("get modifier group", err)
	}

	merged := domain.ModifierGroupInput{
		ItemID:    current.ItemID,
		Name:      mustText(current.Name),
		MinSelect: current.MinSelect,
		MaxSelect: current.MaxSelect,
		SortOrder: current.SortOrder,
	}
	if patch.Name != nil {
		merged.Name = patch.Name.Normalize()
	}
	if patch.MinSelect != nil {
		merged.MinSelect = *patch.MinSelect
	}
	if patch.MaxSelect != nil {
		merged.MaxSelect = *patch.MaxSelect
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if err := merged.Validate(); err != nil {
		return domain.ModifierGroup{}, err
	}

	// A required group with more required picks than it has options would be
	// impossible for a guest to satisfy — they would be stuck unable to order.
	if merged.MinSelect > 0 {
		count, err := s.store.CountModifiersInGroup(ctx, gen.CountModifiersInGroupParams{
			GroupID: id, RestaurantID: restaurantID,
		})
		if err != nil {
			return domain.ModifierGroup{}, wrap("count modifiers", err)
		}
		if int64(merged.MinSelect) > count {
			var v domain.Validator
			v.Add("min_select", "is %d but the group has only %d options", merged.MinSelect, count)
			return domain.ModifierGroup{}, v.Err()
		}
	}

	params := gen.UpdateModifierGroupParams{
		ID:           id,
		RestaurantID: restaurantID,
		MinSelect:    patch.MinSelect,
		MaxSelect:    patch.MaxSelect,
		SortOrder:    patch.SortOrder,
	}
	if patch.Name != nil {
		if params.Name, err = merged.Name.Raw(); err != nil {
			return domain.ModifierGroup{}, err
		}
	}

	var row gen.ModifierGroup
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.UpdateModifierGroup(ctx, params)
		return wrap("update modifier group", err)
	})
	if err != nil {
		return domain.ModifierGroup{}, err
	}
	return postgres.ToModifierGroup(row)
}

func (s *Menu) DeleteModifierGroup(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.DeleteModifierGroup(ctx, gen.DeleteModifierGroupParams{ID: id, RestaurantID: restaurantID})
		if err != nil {
			return wrap("delete modifier group", err)
		}
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

func (s *Menu) CreateModifier(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.ModifierInput) (domain.Modifier, error) {
	in.Name = in.Name.Normalize()
	if err := in.Validate(); err != nil {
		return domain.Modifier{}, err
	}

	nameRaw, err := in.Name.Raw()
	if err != nil {
		return domain.Modifier{}, err
	}

	var row gen.Modifier
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.CreateModifier(ctx, gen.CreateModifierParams{
			RestaurantID:    restaurantID,
			GroupID:         in.GroupID,
			Name:            nameRaw,
			PriceDeltaMinor: in.PriceDeltaMinor,
			SortOrder:       in.SortOrder,
		})
		return wrap("create modifier", err)
	})
	if err != nil {
		return domain.Modifier{}, err
	}
	return postgres.ToModifier(row)
}

func (s *Menu) ListModifiers(ctx context.Context, tenantID, restaurantID, groupID uuid.UUID) ([]domain.Modifier, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListModifiers(ctx, gen.ListModifiersParams{GroupID: groupID, RestaurantID: restaurantID})
	if err != nil {
		return nil, wrap("list modifiers", err)
	}
	return postgres.ToModifiers(rows)
}

type ModifierPatch struct {
	Name            *i18n.Text
	PriceDeltaMinor *int64
	IsAvailable     *bool
	SortOrder       *int32
}

func (s *Menu) UpdateModifier(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch ModifierPatch) (domain.Modifier, error) {
	if err := s.read(ctx, tenantID, restaurantID); err != nil {
		return domain.Modifier{}, err
	}
	current, err := s.store.GetModifier(ctx, gen.GetModifierParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Modifier{}, wrap("get modifier", err)
	}

	merged := domain.ModifierInput{
		GroupID:         current.GroupID,
		Name:            mustText(current.Name),
		PriceDeltaMinor: current.PriceDeltaMinor,
		SortOrder:       current.SortOrder,
	}
	if patch.Name != nil {
		merged.Name = patch.Name.Normalize()
	}
	if patch.PriceDeltaMinor != nil {
		merged.PriceDeltaMinor = *patch.PriceDeltaMinor
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if err := merged.Validate(); err != nil {
		return domain.Modifier{}, err
	}

	params := gen.UpdateModifierParams{
		ID:              id,
		RestaurantID:    restaurantID,
		PriceDeltaMinor: patch.PriceDeltaMinor,
		IsAvailable:     patch.IsAvailable,
		SortOrder:       patch.SortOrder,
	}
	if patch.Name != nil {
		if params.Name, err = merged.Name.Raw(); err != nil {
			return domain.Modifier{}, err
		}
	}

	var row gen.Modifier
	err = s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		var err error
		row, err = q.UpdateModifier(ctx, params)
		return wrap("update modifier", err)
	})
	if err != nil {
		return domain.Modifier{}, err
	}
	return postgres.ToModifier(row)
}

func (s *Menu) DeleteModifier(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	return s.write(ctx, tenantID, restaurantID, func(q *gen.Queries) error {
		n, err := q.DeleteModifier(ctx, gen.DeleteModifierParams{ID: id, RestaurantID: restaurantID})
		if err != nil {
			return wrap("delete modifier", err)
		}
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- helpers

// wrap adds context and maps driver errors to domain sentinels in one step.
func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, postgres.MapError(err))
}

func rawPair(name, description i18n.Text) (nameRaw, descRaw []byte, err error) {
	if nameRaw, err = name.Raw(); err != nil {
		return nil, nil, err
	}
	if descRaw, err = description.Raw(); err != nil {
		return nil, nil, err
	}
	return nameRaw, descRaw, nil
}

// mustText decodes a stored translation for merge purposes. The value came out
// of our own jsonb column, so a decode failure means the row is corrupt; an
// empty Text then fails validation and the update is rejected, which is the
// right outcome either way.
func mustText(raw []byte) i18n.Text {
	t, err := i18n.FromRaw(raw)
	if err != nil {
		return i18n.Text{}
	}
	return t
}
