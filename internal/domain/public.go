package domain

import "slices"

// The public menu is a different shape from the admin one, on purpose.
//
// Admin responses carry every translation, because that is the editing surface.
// Public responses carry exactly one language, already resolved. A four-language
// menu with descriptions is roughly four times the bytes, and a guest on
// restaurant wifi reads one of them. Resolving server-side also puts the
// fallback rules in one place instead of every client.
//
// Nothing in these structs is time-dependent. That is what lets the whole
// payload be cached under a key that only changes when the menu changes. The
// serving-window fields are passed through so the *renderer* can decide what is
// available right now.

type PublicMenu struct {
	Locale     string            `json:"locale"`
	Restaurant PublicRestaurant  `json:"restaurant"`
	Menus      []PublicMenuGroup `json:"menus"`
}

type PublicRestaurant struct {
	ID            string         `json:"id"`
	Slug          string         `json:"slug"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Currency      string         `json:"currency"`
	Timezone      string         `json:"timezone"`
	DefaultLocale string         `json:"default_locale"`
	Locales       []string       `json:"locales"`
	Phone         *string        `json:"phone"`
	Address       *string        `json:"address"`
	Theme         map[string]any `json:"theme"`
	MenuVersion   int32          `json:"menu_version"`
	Logo          *PublicImage   `json:"logo"`
	Cover         *PublicImage   `json:"cover"`
}

type PublicMenuGroup struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Description      string           `json:"description"`
	AvailableFromMin *int32           `json:"available_from_min"`
	AvailableToMin   *int32           `json:"available_to_min"`
	Days             []int16          `json:"days"`
	Categories       []PublicCategory `json:"categories"`
}

type PublicCategory struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Items       []PublicItem `json:"items"`
}

// A sold-out item is included and flagged rather than hidden: a guest looking
// for the dish they came for should see that it exists and is unavailable
// today, not silently fail to find it.
type PublicItem struct {
	ID             string                `json:"id"`
	Name           string                `json:"name"`
	Description    string                `json:"description"`
	PriceMinor     int64                 `json:"price_minor"`
	IsAvailable    bool                  `json:"is_available"`
	Tags           []string              `json:"tags"`
	Allergens      []string              `json:"allergens"`
	Calories       *int32                `json:"calories"`
	PrepMinutes    *int32                `json:"prep_minutes"`
	Variants       []PublicVariant       `json:"variants"`
	ModifierGroups []PublicModifierGroup `json:"modifier_groups"`
	// Nil when the dish has no photo, which is the common case for a menu that
	// has just been typed in. The page has to read well either way.
	Image *PublicImage `json:"image"`
}

type PublicVariant struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	PriceDeltaMinor int64  `json:"price_delta_minor"`
	IsDefault       bool   `json:"is_default"`
	IsAvailable     bool   `json:"is_available"`
}

type PublicModifierGroup struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	MinSelect int32            `json:"min_select"`
	MaxSelect int32            `json:"max_select"`
	Modifiers []PublicModifier `json:"modifiers"`
}

type PublicModifier struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	PriceDeltaMinor int64  `json:"price_delta_minor"`
	IsAvailable     bool   `json:"is_available"`
}

// PublicTable is the table context attached when a guest arrives via a QR code
// rather than the restaurant's plain menu URL.
type PublicTable struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ResolveLocale picks the language to serve.
//
// An unknown or unsupported locale falls back to the restaurant's default
// rather than erroring: a guest whose phone asks for a language the restaurant
// does not publish should still get a menu.
func ResolveLocale(requested string, supported []string, defaultLocale string) string {
	if requested != "" && slices.Contains(supported, requested) {
		return requested
	}
	return defaultLocale
}
