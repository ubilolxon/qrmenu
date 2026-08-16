package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// tree builds a menu → category → item chain and returns the ids.
type tree struct {
	restaurantID uuid.UUID
	menu         domain.Menu
	category     domain.Category
	item         domain.Item
}

func (h *harness) tree(t *testing.T, tenantID uuid.UUID) tree {
	t.Helper()
	ctx := context.Background()

	restaurant := h.restaurant(t, tenantID)

	menu, err := h.menus.CreateMenu(ctx, tenantID, restaurant.ID, domain.MenuInput{
		Name: i18n.Text{"uz": "Asosiy", "ru": "Основное"},
	})
	if err != nil {
		t.Fatalf("create menu: %v", err)
	}

	category, err := h.menus.CreateCategory(ctx, tenantID, restaurant.ID, domain.CategoryInput{
		MenuID: menu.ID,
		Name:   i18n.Text{"uz": "Salatlar", "ru": "Салаты"},
	})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}

	item, err := h.menus.CreateItem(ctx, tenantID, restaurant.ID, domain.ItemInput{
		CategoryID: category.ID,
		Name:       i18n.Text{"uz": "Achichuk", "ru": "Ачичук"},
		PriceMinor: 25000,
		Tags:       []string{"vegan", "spicy"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	return tree{restaurantID: restaurant.ID, menu: menu, category: category, item: item}
}

func TestMenuTreeRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	if tr.item.PriceMinor != 25000 {
		t.Errorf("price_minor = %d, want 25000", tr.item.PriceMinor)
	}
	if len(tr.item.Tags) != 2 {
		t.Errorf("tags = %v, want 2 entries", tr.item.Tags)
	}

	items, err := h.menus.ListItems(ctx, owner.User.TenantID, tr.restaurantID, tr.category.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != tr.item.ID {
		t.Fatalf("list items returned %d rows, want the one just created", len(items))
	}

	// Variants and modifiers hang off the item.
	variant, err := h.menus.CreateVariant(ctx, owner.User.TenantID, tr.restaurantID, domain.VariantInput{
		ItemID: tr.item.ID, Name: i18n.Text{"uz": "Katta"}, PriceDeltaMinor: 5000, IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create variant: %v", err)
	}
	if !variant.IsDefault {
		t.Error("variant was not marked default")
	}

	group, err := h.menus.CreateModifierGroup(ctx, owner.User.TenantID, tr.restaurantID, domain.ModifierGroupInput{
		ItemID: tr.item.ID, Name: i18n.Text{"uz": "Qo'shimchalar"}, MinSelect: 0, MaxSelect: 3,
	})
	if err != nil {
		t.Fatalf("create modifier group: %v", err)
	}
	if _, err := h.menus.CreateModifier(ctx, owner.User.TenantID, tr.restaurantID, domain.ModifierInput{
		GroupID: group.ID, Name: i18n.Text{"uz": "Qaymoq"}, PriceDeltaMinor: 3000,
	}); err != nil {
		t.Fatalf("create modifier: %v", err)
	}

	modifiers, err := h.menus.ListModifiers(ctx, owner.User.TenantID, tr.restaurantID, group.ID)
	if err != nil {
		t.Fatalf("list modifiers: %v", err)
	}
	if len(modifiers) != 1 {
		t.Errorf("got %d modifiers, want 1", len(modifiers))
	}
}

// The same assertion as the restaurant suite, extended over the whole menu
// tree: nothing a second tenant does can reach the first tenant's rows.
func TestMenuTreeRejectsForeignTenant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	bob := h.tenant(t)
	tr := h.tree(t, alice.User.TenantID)

	newName := i18n.Text{"uz": "Buzilgan"}
	available := false

	operations := map[string]func() error{
		"ListMenus": func() error {
			_, err := h.menus.ListMenus(ctx, bob.User.TenantID, tr.restaurantID)
			return err
		},
		"GetMenu": func() error {
			_, err := h.menus.GetMenu(ctx, bob.User.TenantID, tr.restaurantID, tr.menu.ID)
			return err
		},
		"UpdateMenu": func() error {
			_, err := h.menus.UpdateMenu(ctx, bob.User.TenantID, tr.restaurantID, tr.menu.ID,
				service.MenuPatch{Name: &newName})
			return err
		},
		"DeleteMenu": func() error {
			return h.menus.DeleteMenu(ctx, bob.User.TenantID, tr.restaurantID, tr.menu.ID)
		},
		"GetCategory": func() error {
			_, err := h.menus.GetCategory(ctx, bob.User.TenantID, tr.restaurantID, tr.category.ID)
			return err
		},
		"UpdateCategory": func() error {
			_, err := h.menus.UpdateCategory(ctx, bob.User.TenantID, tr.restaurantID, tr.category.ID,
				service.CategoryPatch{Name: &newName})
			return err
		},
		"DeleteCategory": func() error {
			return h.menus.DeleteCategory(ctx, bob.User.TenantID, tr.restaurantID, tr.category.ID)
		},
		"GetItem": func() error {
			_, err := h.menus.GetItem(ctx, bob.User.TenantID, tr.restaurantID, tr.item.ID)
			return err
		},
		"UpdateItem": func() error {
			_, err := h.menus.UpdateItem(ctx, bob.User.TenantID, tr.restaurantID, tr.item.ID,
				service.ItemPatch{Name: &newName})
			return err
		},
		"SetItemAvailability": func() error {
			_, err := h.menus.SetItemAvailability(ctx, bob.User.TenantID, tr.restaurantID, tr.item.ID, available)
			return err
		},
		"DeleteItem": func() error {
			return h.menus.DeleteItem(ctx, bob.User.TenantID, tr.restaurantID, tr.item.ID)
		},
		"ReorderMenus": func() error {
			return h.menus.ReorderMenus(ctx, bob.User.TenantID, tr.restaurantID, []uuid.UUID{tr.menu.ID})
		},
		"ReorderItems": func() error {
			return h.menus.ReorderItems(ctx, bob.User.TenantID, tr.restaurantID, []uuid.UUID{tr.item.ID})
		},
	}

	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("%s as a foreign tenant returned %v, want ErrNotFound", name, err)
			}
		})
	}

	// Untouched afterwards.
	item, err := h.menus.GetItem(ctx, alice.User.TenantID, tr.restaurantID, tr.item.ID)
	if err != nil {
		t.Fatalf("owner lost access to their own item: %v", err)
	}
	if !item.IsAvailable || item.Name.Get("uz", "uz") != "Achichuk" {
		t.Error("item was modified by a foreign tenant")
	}
}

// The case the composite foreign keys exist for: one tenant, two restaurants.
// The tenant check passes for both, so only the (parent_id, restaurant_id)
// reference stops a category from being attached to a sibling's menu.
func TestCannotAttachToAnotherRestaurantsParent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	first := h.tree(t, owner.User.TenantID)
	second := h.tree(t, owner.User.TenantID)

	t.Run("category under a sibling restaurant's menu", func(t *testing.T) {
		_, err := h.menus.CreateCategory(ctx, owner.User.TenantID, second.restaurantID, domain.CategoryInput{
			MenuID: first.menu.ID, // belongs to the other restaurant
			Name:   i18n.Text{"uz": "Yomon"},
		})
		if err == nil {
			t.Fatal("created a category under another restaurant's menu")
		}
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("item under a sibling restaurant's category", func(t *testing.T) {
		_, err := h.menus.CreateItem(ctx, owner.User.TenantID, second.restaurantID, domain.ItemInput{
			CategoryID: first.category.ID,
			Name:       i18n.Text{"uz": "Yomon"},
			PriceMinor: 1000,
		})
		if err == nil {
			t.Fatal("created an item under another restaurant's category")
		}
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("moving an item into a sibling restaurant's category", func(t *testing.T) {
		_, err := h.menus.UpdateItem(ctx, owner.User.TenantID, second.restaurantID, second.item.ID,
			service.ItemPatch{CategoryID: &first.category.ID})
		if err == nil {
			t.Fatal("moved an item into another restaurant's category")
		}
	})

	// Reading a sibling's row by id, while naming your own restaurant, must
	// also find nothing.
	t.Run("reading across restaurants", func(t *testing.T) {
		_, err := h.menus.GetItem(ctx, owner.User.TenantID, second.restaurantID, first.item.ID)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}

// menu_version is the public cache key suffix, so every menu write must move
// it or Phase 3 will serve a stale menu.
func TestMenuWritesBumpMenuVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	before, err := h.restaurants.Get(ctx, owner.User.TenantID, tr.restaurantID)
	if err != nil {
		t.Fatalf("get restaurant: %v", err)
	}

	if _, err := h.menus.SetItemAvailability(ctx, owner.User.TenantID, tr.restaurantID, tr.item.ID, false); err != nil {
		t.Fatalf("set availability: %v", err)
	}

	after, err := h.restaurants.Get(ctx, owner.User.TenantID, tr.restaurantID)
	if err != nil {
		t.Fatalf("get restaurant: %v", err)
	}
	if after.MenuVersion <= before.MenuVersion {
		t.Errorf("menu_version went %d -> %d; a menu write must bump it",
			before.MenuVersion, after.MenuVersion)
	}
}

// A failed write must not bump the version either — otherwise every rejected
// request would pointlessly invalidate the cache.
func TestFailedWriteDoesNotBumpMenuVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	before, err := h.restaurants.Get(ctx, owner.User.TenantID, tr.restaurantID)
	if err != nil {
		t.Fatalf("get restaurant: %v", err)
	}

	// Deleting a non-existent item rolls the transaction back.
	if err := h.menus.DeleteItem(ctx, owner.User.TenantID, tr.restaurantID, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete of a missing item returned %v, want ErrNotFound", err)
	}

	after, err := h.restaurants.Get(ctx, owner.User.TenantID, tr.restaurantID)
	if err != nil {
		t.Fatalf("get restaurant: %v", err)
	}
	if after.MenuVersion != before.MenuVersion {
		t.Errorf("menu_version moved %d -> %d on a failed write", before.MenuVersion, after.MenuVersion)
	}
}

// A reorder is all-or-nothing: smuggling in an id from another restaurant must
// reject the batch rather than silently reorder the subset that matched.
func TestReorderRejectsForeignIDs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	mine := h.tree(t, owner.User.TenantID)
	other := h.tree(t, owner.User.TenantID)

	err := h.menus.ReorderItems(ctx, owner.User.TenantID, mine.restaurantID,
		[]uuid.UUID{mine.item.ID, other.item.ID})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("reorder with a foreign id returned %v, want ErrNotFound", err)
	}

	// And the legitimate row must be unchanged, proving the rollback.
	item, err := h.menus.GetItem(ctx, owner.User.TenantID, mine.restaurantID, mine.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.SortOrder != mine.item.SortOrder {
		t.Errorf("sort_order changed to %d despite the batch being rejected", item.SortOrder)
	}
}

func TestReorderRejectsDuplicateIDs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	var validationErr *domain.ValidationError
	err := h.menus.ReorderItems(ctx, owner.User.TenantID, tr.restaurantID,
		[]uuid.UUID{tr.item.ID, tr.item.ID})
	if !errors.As(err, &validationErr) {
		t.Fatalf("duplicate ids returned %v, want a validation error", err)
	}
}

// The partial unique index allows one default variant per item, so promoting a
// new default has to demote the old one in the same transaction.
func TestOnlyOneDefaultVariantPerItem(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	first, err := h.menus.CreateVariant(ctx, owner.User.TenantID, tr.restaurantID, domain.VariantInput{
		ItemID: tr.item.ID, Name: i18n.Text{"uz": "Kichik"}, IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create first variant: %v", err)
	}

	second, err := h.menus.CreateVariant(ctx, owner.User.TenantID, tr.restaurantID, domain.VariantInput{
		ItemID: tr.item.ID, Name: i18n.Text{"uz": "Katta"}, IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create second default variant: %v", err)
	}

	variants, err := h.menus.ListVariants(ctx, owner.User.TenantID, tr.restaurantID, tr.item.ID)
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}

	defaults := 0
	for _, v := range variants {
		if v.IsDefault {
			defaults++
			if v.ID != second.ID {
				t.Errorf("default is %s, want the most recently promoted %s", v.ID, second.ID)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("%d default variants, want exactly 1", defaults)
	}
	_ = first
}

// A required modifier group with fewer options than required picks would leave
// a guest unable to complete an order.
func TestModifierGroupCannotRequireMoreThanItHas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	group, err := h.menus.CreateModifierGroup(ctx, owner.User.TenantID, tr.restaurantID, domain.ModifierGroupInput{
		ItemID: tr.item.ID, Name: i18n.Text{"uz": "Sous"}, MinSelect: 0, MaxSelect: 2,
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	// The group has no options yet, so requiring one is impossible to satisfy.
	minSelect := int32(1)
	var validationErr *domain.ValidationError
	_, err = h.menus.UpdateModifierGroup(ctx, owner.User.TenantID, tr.restaurantID, group.ID,
		service.ModifierGroupPatch{MinSelect: &minSelect})
	if !errors.As(err, &validationErr) {
		t.Fatalf("got %v, want a validation error", err)
	}

	// After adding an option it becomes satisfiable.
	if _, err := h.menus.CreateModifier(ctx, owner.User.TenantID, tr.restaurantID, domain.ModifierInput{
		GroupID: group.ID, Name: i18n.Text{"uz": "Achchiq"},
	}); err != nil {
		t.Fatalf("create modifier: %v", err)
	}
	if _, err := h.menus.UpdateModifierGroup(ctx, owner.User.TenantID, tr.restaurantID, group.ID,
		service.ModifierGroupPatch{MinSelect: &minSelect}); err != nil {
		t.Fatalf("update after adding an option: %v", err)
	}
}

// Deleting a menu must take its categories and items with it, or the next
// public menu build would trip over rows with no parent.
func TestDeletingMenuCascades(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	if err := h.menus.DeleteMenu(ctx, owner.User.TenantID, tr.restaurantID, tr.menu.ID); err != nil {
		t.Fatalf("delete menu: %v", err)
	}

	if _, err := h.menus.GetCategory(ctx, owner.User.TenantID, tr.restaurantID, tr.category.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("category survived its menu: %v", err)
	}
	if _, err := h.menus.GetItem(ctx, owner.User.TenantID, tr.restaurantID, tr.item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("item survived its menu: %v", err)
	}
}

func TestItemPatchLeavesUnsentFieldsAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	owner := h.tenant(t)
	tr := h.tree(t, owner.User.TenantID)

	newPrice := int64(31000)
	updated, err := h.menus.UpdateItem(ctx, owner.User.TenantID, tr.restaurantID, tr.item.ID,
		service.ItemPatch{PriceMinor: &newPrice})
	if err != nil {
		t.Fatalf("update item: %v", err)
	}

	if updated.PriceMinor != newPrice {
		t.Errorf("price_minor = %d, want %d", updated.PriceMinor, newPrice)
	}
	if updated.Name.Get("uz", "uz") != "Achichuk" {
		t.Errorf("name = %q, want it untouched", updated.Name.Get("uz", "uz"))
	}
	if len(updated.Tags) != 2 {
		t.Errorf("tags = %v, want them untouched", updated.Tags)
	}
}
