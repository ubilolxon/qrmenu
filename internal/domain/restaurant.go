package domain

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/i18n"
)

type Restaurant struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Slug          string
	Name          i18n.Text
	Description   i18n.Text
	Currency      string
	Timezone      string
	DefaultLocale string
	Locales       []string
	Phone         *string
	Address       *string
	Theme         map[string]any
	LogoImageID   *uuid.UUID
	CoverImageID  *uuid.UUID
	MenuVersion   int32
	IsActive      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const (
	MinSlugLen = 3
	MaxSlugLen = 63
)

var (
	slugPattern     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	localePattern   = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)
)

// reservedSlugs are names the platform needs for its own routes and for
// operational hostnames. Handing "api" to a restaurant would collide the moment
// custom domains ship.
var reservedSlugs = []string{
	"api", "admin", "www", "app", "auth", "login", "logout", "register",
	"static", "assets", "media", "images", "cdn", "menu", "menus", "public",
	"dashboard", "billing", "support", "help", "docs", "status", "health",
	"webhooks", "staff", "kitchen", "order", "orders", "qr", "t", "m",
}

// NormalizeSlug lowercases and trims, so "  Chaykhana " and "chaykhana" cannot
// both be claimed.
func NormalizeSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// ValidateSlug checks the public menu identifier. It appears in the QR URL, so
// it must be short, unambiguous, and safe in a hostname if custom domains ship.
func ValidateSlug(v *Validator, slug string) {
	switch {
	case slug == "":
		v.Add("slug", "is required")
	case len(slug) < MinSlugLen:
		v.Add("slug", "must be at least %d characters", MinSlugLen)
	case len(slug) > MaxSlugLen:
		v.Add("slug", "must be at most %d characters", MaxSlugLen)
	case !slugPattern.MatchString(slug):
		v.Add("slug", "may contain only lowercase letters, digits and single hyphens between them")
	case slices.Contains(reservedSlugs, slug):
		v.Add("slug", "%q is reserved", slug)
	}
}

// RestaurantInput is the validated shape for creating a restaurant. Update uses
// the same rules per field, but only for fields the client actually sent.
type RestaurantInput struct {
	Slug          string
	Name          i18n.Text
	Description   i18n.Text
	Currency      string
	Timezone      string
	DefaultLocale string
	Locales       []string
	Phone         *string
	Address       *string
}

// Validate checks a complete restaurant. Every rule here is also enforced by a
// database constraint where it is cheap to do so; this layer exists to produce
// a good error message, not to be the only guard.
func (in RestaurantInput) Validate() error {
	var v Validator

	ValidateSlug(&v, in.Slug)
	ValidateTranslated(&v, "name", in.Name, true)
	ValidateTranslated(&v, "description", in.Description, false)
	ValidateCurrency(&v, in.Currency)
	ValidateTimezone(&v, in.Timezone)
	ValidateLocales(&v, in.Locales, in.DefaultLocale)

	if in.Phone != nil && utf8.RuneCountInString(*in.Phone) > 32 {
		v.Add("phone", "must be at most 32 characters")
	}
	if in.Address != nil && utf8.RuneCountInString(*in.Address) > 500 {
		v.Add("address", "must be at most 500 characters")
	}

	return v.Err()
}

// ValidateTranslated checks an i18n field. Pass required=false for fields such
// as description that may legitimately be absent entirely.
func ValidateTranslated(v *Validator, field string, t i18n.Text, required bool) {
	if t.IsEmpty() {
		if required {
			v.Add(field, "must have a translation in at least one language")
		}
		return
	}
	if err := t.Validate(); err != nil {
		v.Add(field, "%s", err.Error())
	}
}

func ValidateCurrency(v *Validator, currency string) {
	if !currencyPattern.MatchString(currency) {
		v.Add("currency", "must be a 3-letter ISO 4217 code such as UZS or USD")
	}
}

// ValidateTimezone rejects anything the Go runtime cannot load, because order
// timestamps and menu schedules are rendered in the restaurant's local time.
func ValidateTimezone(v *Validator, tz string) {
	if tz == "" {
		v.Add("timezone", "is required")
		return
	}
	if _, err := time.LoadLocation(tz); err != nil {
		v.Add("timezone", "%q is not a known IANA timezone", tz)
	}
}

// ValidateLocales enforces the rule the database also carries: the default
// locale must be one the restaurant actually publishes, or every fallback
// lookup dead-ends.
func ValidateLocales(v *Validator, locales []string, defaultLocale string) {
	if len(locales) == 0 {
		v.Add("locales", "must list at least one language")
		return
	}
	if len(locales) > i18n.MaxLocales {
		v.Add("locales", "must list at most %d languages", i18n.MaxLocales)
	}

	seen := make(map[string]bool, len(locales))
	for _, l := range locales {
		if !localePattern.MatchString(l) {
			v.Add("locales", "%q is not a valid locale code", l)
			continue
		}
		if seen[l] {
			v.Add("locales", "%q is listed twice", l)
		}
		seen[l] = true
	}

	switch {
	case defaultLocale == "":
		v.Add("default_locale", "is required")
	case !seen[defaultLocale]:
		v.Add("default_locale", "%q must be one of the listed locales", defaultLocale)
	}
}
