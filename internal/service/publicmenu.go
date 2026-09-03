package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// PublicMenu serves the guest-facing menu. This is the hot path: roughly all
// the traffic, read-only, and spiky at meal times.
type PublicMenu struct {
	store  *postgres.Store
	cache  *redis.Client
	ttl    time.Duration
	logger *slog.Logger
	media  MediaURLs

	// Counted here rather than reported to a metrics package, so this file
	// stays free of a Prometheus import; internal/metrics reads them when a
	// scrape arrives. The hit ratio is the best health signal the service has —
	// a hit is one Redis round trip, a miss is a dozen queries and a render.
	hits   atomic.Uint64
	misses atomic.Uint64
}

// CacheHits and CacheMisses satisfy metrics.CacheSource.
func (s *PublicMenu) CacheHits() uint64   { return s.hits.Load() }
func (s *PublicMenu) CacheMisses() uint64 { return s.misses.Load() }

// MediaURLs turns a storage key into the address a browser fetches it from.
//
// A narrow interface rather than the storage client itself: assembling a menu
// needs to name an object, not to read, write or delete one, and a test should
// not have to stand up a bucket to render a page.
type MediaURLs interface {
	PublicURL(key string) string
}

// DefaultCacheTTL is a backstop, not the invalidation mechanism. Correctness
// comes from menu_version being part of the cache key, so a stale entry is
// never read after a write. The TTL only stops superseded keys accumulating.
const DefaultCacheTTL = 24 * time.Hour

// media may be nil, in which case the menu is assembled without images. That is
// the degraded mode for a deployment with no object storage configured: a menu
// with no photos still serves.
func NewPublicMenu(store *postgres.Store, cache *redis.Client, logger *slog.Logger, media MediaURLs) *PublicMenu {
	return &PublicMenu{store: store, cache: cache, ttl: DefaultCacheTTL, logger: logger, media: media}
}

// BySlug serves the restaurant's plain menu URL.
func (s *PublicMenu) BySlug(ctx context.Context, slug, requestedLocale string) (*domain.PublicMenu, error) {
	restaurant, err := s.store.GetRestaurantBySlug(ctx, domain.NormalizeSlug(slug))
	if err != nil {
		return nil, wrap("get restaurant", err)
	}
	return s.build(ctx, restaurant, requestedLocale)
}

// ByToken serves a QR scan, returning the table context alongside the menu.
func (s *PublicMenu) ByToken(ctx context.Context, token, requestedLocale string) (*domain.PublicMenu, *domain.PublicTable, error) {
	token = domain.NormalizeQRToken(token)
	if !domain.ValidQRTokenShape(token) {
		// Rejecting an impossible token here keeps malformed scans off the
		// database entirely.
		return nil, nil, domain.ErrNotFound
	}

	row, err := s.store.GetTableByToken(ctx, token)
	if err != nil {
		return nil, nil, wrap("get table", err)
	}

	menu, err := s.build(ctx, row.Restaurant, requestedLocale)
	if err != nil {
		return nil, nil, err
	}

	return menu, &domain.PublicTable{
		ID:    row.RestaurantTable.ID.String(),
		Label: row.RestaurantTable.Label,
	}, nil
}

// CacheKey is the cache identity of one rendered menu.
//
// menu_version is part of the key rather than something to invalidate. A write
// bumps the counter, which means every old key is simply never read again — no
// invalidation logic, no keys to hunt down, no window where a stale entry is
// still reachable. Superseded keys expire on their own.
func CacheKey(restaurantID uuid.UUID, locale string, menuVersion int32) string {
	return fmt.Sprintf("menu:%s:%s:v%d", restaurantID, locale, menuVersion)
}

func (s *PublicMenu) build(ctx context.Context, restaurant gen.Restaurant, requestedLocale string) (*domain.PublicMenu, error) {
	locale := domain.ResolveLocale(requestedLocale, restaurant.Locales, restaurant.DefaultLocale)
	key := CacheKey(restaurant.ID, locale, restaurant.MenuVersion)

	if cached, ok := s.fromCache(ctx, key); ok {
		s.hits.Add(1)
		return cached, nil
	}
	// Counted once here rather than at each of the three ways fromCache can
	// come back empty, so "miss" means the same thing on every path: the menu
	// had to be rebuilt.
	s.misses.Add(1)

	menu, err := s.assemble(ctx, restaurant, locale)
	if err != nil {
		return nil, err
	}

	s.toCache(ctx, key, menu)
	return menu, nil
}

// fromCache reads a rendered menu. Every failure path — Redis down, corrupt
// entry — is a miss, never an error: the cache is a performance dependency, and
// the database can always answer.
func (s *PublicMenu) fromCache(ctx context.Context, key string) (*domain.PublicMenu, bool) {
	if s.cache == nil {
		return nil, false
	}

	raw, err := s.cache.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.logger.WarnContext(ctx, "menu cache read failed",
				slog.String("key", key), slog.Any("error", err))
		}
		return nil, false
	}

	var menu domain.PublicMenu
	if err := json.Unmarshal(raw, &menu); err != nil {
		s.logger.WarnContext(ctx, "menu cache entry is corrupt, rebuilding",
			slog.String("key", key), slog.Any("error", err))
		return nil, false
	}
	return &menu, true
}

func (s *PublicMenu) toCache(ctx context.Context, key string, menu *domain.PublicMenu) {
	if s.cache == nil {
		return
	}
	raw, err := json.Marshal(menu)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not encode menu for cache", slog.Any("error", err))
		return
	}
	if err := s.cache.Set(ctx, key, raw, s.ttl).Err(); err != nil {
		s.logger.WarnContext(ctx, "menu cache write failed",
			slog.String("key", key), slog.Any("error", err))
	}
}

// assemble builds the whole menu from six flat reads and stitches the tree in
// memory.
//
// Six queries by restaurant_id, each an index scan, beats walking the tree with
// a query per category and per item — that shape is where menu endpoints
// usually die, at a few hundred round trips for a large menu.
func (s *PublicMenu) assemble(ctx context.Context, restaurant gen.Restaurant, locale string) (*domain.PublicMenu, error) {
	id := restaurant.ID

	menuRows, err := s.store.ListMenus(ctx, id)
	if err != nil {
		return nil, wrap("list menus", err)
	}
	categoryRows, err := s.store.ListCategoriesByRestaurant(ctx, id)
	if err != nil {
		return nil, wrap("list categories", err)
	}
	itemRows, err := s.store.ListItemsByRestaurant(ctx, id)
	if err != nil {
		return nil, wrap("list items", err)
	}
	variantRows, err := s.store.ListVariantsByRestaurant(ctx, id)
	if err != nil {
		return nil, wrap("list variants", err)
	}
	groupRows, err := s.store.ListModifierGroupsByRestaurant(ctx, id)
	if err != nil {
		return nil, wrap("list modifier groups", err)
	}
	modifierRows, err := s.store.ListModifiersByRestaurant(ctx, id)
	if err != nil {
		return nil, wrap("list modifiers", err)
	}
	// Only ready images are returned, so a pending or rejected upload cannot
	// reach a guest's page even if something has already attached it.
	imageRows, err := s.store.ListReadyImages(ctx, id)
	if err != nil {
		return nil, wrap("list images", err)
	}

	imagesByID := make(map[uuid.UUID]domain.Image, len(imageRows))
	for _, row := range imageRows {
		imagesByID[row.ID] = postgres.ToImage(row)
	}
	// picture resolves an attachment to the public form, and returns nil for
	// anything missing, unfinished or unresolvable — a half-built image must
	// never render as a broken box on a menu.
	picture := func(imageID *uuid.UUID) *domain.PublicImage {
		if imageID == nil || s.media == nil {
			return nil
		}
		img, ok := imagesByID[*imageID]
		if !ok {
			return nil
		}
		return img.ToPublic(s.media.PublicURL)
	}

	fallback := restaurant.DefaultLocale
	text := func(raw []byte) string {
		t, err := i18n.FromRaw(raw)
		if err != nil {
			return ""
		}
		return t.Get(locale, fallback)
	}

	// Index children by parent so the tree is built in one pass rather than a
	// nested scan per level.
	modifiersByGroup := make(map[uuid.UUID][]domain.PublicModifier, len(groupRows))
	for _, m := range modifierRows {
		modifiersByGroup[m.GroupID] = append(modifiersByGroup[m.GroupID], domain.PublicModifier{
			ID:              m.ID.String(),
			Name:            text(m.Name),
			PriceDeltaMinor: m.PriceDeltaMinor,
			IsAvailable:     m.IsAvailable,
		})
	}

	groupsByItem := make(map[uuid.UUID][]domain.PublicModifierGroup)
	for _, g := range groupRows {
		groupsByItem[g.ItemID] = append(groupsByItem[g.ItemID], domain.PublicModifierGroup{
			ID:        g.ID.String(),
			Name:      text(g.Name),
			MinSelect: g.MinSelect,
			MaxSelect: g.MaxSelect,
			Modifiers: orEmptySlice(modifiersByGroup[g.ID]),
		})
	}

	variantsByItem := make(map[uuid.UUID][]domain.PublicVariant)
	for _, v := range variantRows {
		variantsByItem[v.ItemID] = append(variantsByItem[v.ItemID], domain.PublicVariant{
			ID:              v.ID.String(),
			Name:            text(v.Name),
			PriceDeltaMinor: v.PriceDeltaMinor,
			IsDefault:       v.IsDefault,
			IsAvailable:     v.IsAvailable,
		})
	}

	itemsByCategory := make(map[uuid.UUID][]domain.PublicItem)
	for _, it := range itemRows {
		itemsByCategory[it.CategoryID] = append(itemsByCategory[it.CategoryID], domain.PublicItem{
			ID:             it.ID.String(),
			Name:           text(it.Name),
			Description:    text(it.Description),
			PriceMinor:     it.PriceMinor,
			IsAvailable:    it.IsAvailable,
			Tags:           orEmptyStrings(it.Tags),
			Allergens:      orEmptyStrings(it.Allergens),
			Calories:       it.Calories,
			PrepMinutes:    it.PrepMinutes,
			Variants:       orEmptySlice(variantsByItem[it.ID]),
			ModifierGroups: orEmptySlice(groupsByItem[it.ID]),
			Image:          picture(it.ImageID),
		})
	}

	categoriesByMenu := make(map[uuid.UUID][]domain.PublicCategory)
	for _, c := range categoryRows {
		// An unpublished category and everything under it stays hidden.
		if !c.IsActive {
			continue
		}
		categoriesByMenu[c.MenuID] = append(categoriesByMenu[c.MenuID], domain.PublicCategory{
			ID:          c.ID.String(),
			Name:        text(c.Name),
			Description: text(c.Description),
			Items:       orEmptySlice(itemsByCategory[c.ID]),
		})
	}

	menus := make([]domain.PublicMenuGroup, 0, len(menuRows))
	for _, m := range menuRows {
		if !m.IsActive {
			continue
		}
		menus = append(menus, domain.PublicMenuGroup{
			ID:               m.ID.String(),
			Name:             text(m.Name),
			Description:      text(m.Description),
			AvailableFromMin: m.AvailableFromMin,
			AvailableToMin:   m.AvailableToMin,
			Days:             orEmptyDays(m.Days),
			Categories:       orEmptySlice(categoriesByMenu[m.ID]),
		})
	}

	description, err := i18n.FromRaw(restaurant.Description)
	if err != nil {
		description = i18n.Text{}
	}
	name, err := i18n.FromRaw(restaurant.Name)
	if err != nil {
		name = i18n.Text{}
	}

	theme := map[string]any{}
	if len(restaurant.Theme) > 0 {
		_ = json.Unmarshal(restaurant.Theme, &theme)
	}

	return &domain.PublicMenu{
		Locale: locale,
		Restaurant: domain.PublicRestaurant{
			ID:            restaurant.ID.String(),
			Slug:          restaurant.Slug,
			Name:          name.Get(locale, fallback),
			Description:   description.Get(locale, fallback),
			Currency:      restaurant.Currency,
			Timezone:      restaurant.Timezone,
			DefaultLocale: restaurant.DefaultLocale,
			Locales:       restaurant.Locales,
			Phone:         restaurant.Phone,
			Address:       restaurant.Address,
			Theme:         theme,
			MenuVersion:   restaurant.MenuVersion,
			Logo:          picture(restaurant.LogoImageID),
			Cover:         picture(restaurant.CoverImageID),
		},
		Menus: menus,
	}, nil
}

// The JSON encoding must never contain null where a client expects a list.
func orEmptySlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func orEmptyStrings(s []string) []string { return orEmptySlice(s) }
func orEmptyDays(s []int16) []int16      { return orEmptySlice(s) }
