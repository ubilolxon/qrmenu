// Package i18n holds the translated-string type used by every menu-facing
// field.
//
// Translations are stored as a jsonb object per field ({"uz": "Osh",
// "ru": "Плов"}) rather than in a translations table, so adding a language is a
// data change and the whole menu still loads in one query. The cost is that
// nothing in the database enforces a shape, which is what this package exists
// to do.
package i18n

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Text maps a BCP-47-ish locale code to a translation.
type Text map[string]string

const (
	// MaxValueLen bounds a single translation. Menu item descriptions are the
	// longest realistic field.
	MaxValueLen = 2000
	// MaxLocales bounds how many languages one field may carry, so a bad
	// client cannot grow a row without limit.
	MaxLocales = 20
)

// Locale codes such as "uz", "ru", "en", "uz-Cyrl", "pt-BR".
var localePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

// Get resolves the best available translation:
//
//  1. the requested locale,
//  2. the restaurant's default locale,
//  3. any remaining translation, chosen in sorted order.
//
// Step 3 keeps the menu readable rather than blank when a restaurant adds a
// language and has not translated every item yet. Sorted order matters: without
// it, Go's random map iteration would show a different language on each request
// for the same item.
func (t Text) Get(locale, defaultLocale string) string {
	if v, ok := t[locale]; ok && v != "" {
		return v
	}
	if v, ok := t[defaultLocale]; ok && v != "" {
		return v
	}

	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if t[k] != "" {
			return t[k]
		}
	}
	return ""
}

// IsEmpty reports whether there is no usable translation at all.
func (t Text) IsEmpty() bool {
	for _, v := range t {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

// Locales lists the locales that actually carry a translation, sorted.
func (t Text) Locales() []string {
	out := make([]string, 0, len(t))
	for k, v := range t {
		if strings.TrimSpace(v) != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Normalize trims every value and drops entries that are empty afterwards, so
// {"ru": "   "} does not masquerade as a Russian translation.
func (t Text) Normalize() Text {
	out := make(Text, len(t))
	for k, v := range t {
		if v = strings.TrimSpace(v); v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

// Validate checks the shape. Call it on Normalize'd input.
func (t Text) Validate() error {
	if len(t) == 0 {
		return fmt.Errorf("must contain at least one translation")
	}
	if len(t) > MaxLocales {
		return fmt.Errorf("has %d locales, maximum is %d", len(t), MaxLocales)
	}
	for locale, v := range t {
		if !localePattern.MatchString(locale) {
			return fmt.Errorf("%q is not a valid locale code", locale)
		}
		if len(v) > MaxValueLen {
			return fmt.Errorf("translation for %q is %d characters, maximum is %d",
				locale, len(v), MaxValueLen)
		}
	}
	return nil
}

// Raw encodes to the jsonb form stored in the database.
func (t Text) Raw() (json.RawMessage, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("encode translations: %w", err)
	}
	return b, nil
}

// FromRaw decodes a jsonb column. An empty or NULL column decodes to an empty
// Text rather than an error, since a nullable translated field is legitimate.
func FromRaw(raw json.RawMessage) (Text, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return Text{}, nil
	}
	var t Text
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode translations: %w", err)
	}
	if t == nil {
		t = Text{}
	}
	return t, nil
}

// MustRaw is for defaults and tests, where the input is a literal.
func MustRaw(t Text) json.RawMessage {
	raw, err := t.Raw()
	if err != nil {
		panic(err)
	}
	return raw
}
