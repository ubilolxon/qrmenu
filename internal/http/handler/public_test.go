package handler

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/money"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// The menu page is the one surface a guest actually sees, and html/template
// resolves almost everything at *render* time: a field that does not exist, or
// a func-typed field the template tries to call, parses cleanly and then aborts
// mid-document. The visible symptom is a page that stops after the heading —
// which is easy to miss and impossible to catch with a compile.
//
// So these tests execute the real template against realistic data and assert on
// the bytes that come out.

func ptr[T any](v T) *T { return &v }

// demoMenu mirrors the seeded demo closely enough to exercise every branch the
// template has: a timed menu, a sold-out dish, variants with a negative delta,
// modifiers, tags and allergens, and an empty category.
func demoMenu() *domain.PublicMenu {
	return &domain.PublicMenu{
		Locale: "uz",
		Restaurant: domain.PublicRestaurant{
			ID:            "018f0000-0000-7000-8000-000000000001",
			Slug:          "choyxona",
			Name:          "Choyxona Navruz",
			Description:   "An'anaviy o'zbek taomlari",
			Currency:      "UZS",
			Timezone:      "Asia/Tashkent",
			DefaultLocale: "uz",
			Locales:       []string{"uz", "ru", "en"},
			Phone:         ptr("+998 71 200 00 00"),
			Address:       ptr("Amir Temur ko'chasi 15"),
			MenuVersion:   7,
		},
		Menus: []domain.PublicMenuGroup{
			{
				ID:               "018f0000-0000-7000-8000-000000000010",
				Name:             "Nonushta",
				AvailableFromMin: ptr(int32(480)),
				AvailableToMin:   ptr(int32(660)),
				Days:             []int16{1, 2, 3, 4, 5},
				Categories: []domain.PublicCategory{{
					ID:   "018f0000-0000-7000-8000-000000000020",
					Name: "Ertalabki",
					Items: []domain.PublicItem{{
						ID:          "018f0000-0000-7000-8000-000000000030",
						Name:        "Qatlama",
						PriceMinor:  15000,
						IsAvailable: true,
						Allergens:   []string{"gluten"},
					}},
				}},
			},
			{
				ID:   "018f0000-0000-7000-8000-000000000011",
				Name: "Asosiy menyu",
				Categories: []domain.PublicCategory{
					{
						ID:   "018f0000-0000-7000-8000-000000000021",
						Name: "Issiq taomlar",
						Items: []domain.PublicItem{
							{
								ID:          "018f0000-0000-7000-8000-000000000031",
								Name:        "Osh",
								Description: "Qo'y go'shti, sabzi va zira bilan",
								PriceMinor:  45000,
								IsAvailable: true,
								Tags:        []string{"halol"},
								Variants: []domain.PublicVariant{
									{Name: "Yarim porsiya", PriceDeltaMinor: -12000, IsAvailable: true},
									{Name: "To'liq porsiya", IsDefault: true, IsAvailable: true},
								},
								ModifierGroups: []domain.PublicModifierGroup{{
									Name:      "Qo'shimchalar",
									MaxSelect: 3,
									Modifiers: []domain.PublicModifier{
										{Name: "Qazi", PriceDeltaMinor: 15000, IsAvailable: true},
										{Name: "Achchiq sous", IsAvailable: true},
										// Hidden: an unavailable modifier is not offered.
										{Name: "Bedana tuxumi", PriceDeltaMinor: 6000},
									},
								}},
							},
							{
								ID:          "018f0000-0000-7000-8000-000000000032",
								Name:        "Norin",
								PriceMinor:  40000,
								IsAvailable: false,
								Allergens:   []string{"gluten"},
							},
						},
					},
					{
						ID:    "018f0000-0000-7000-8000-000000000022",
						Name:  "Ichimliklar",
						Items: nil, // exercises the {{ else }} arm of the item range
					},
				},
			},
		},
	}
}

func renderPageFor(t *testing.T, path string, menu *domain.PublicMenu, table *domain.PublicTable) string {
	t.Helper()

	h, err := NewPublic(service.NewPublicMenu(nil, nil, nil), config.Public{})
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	r := httptest.NewRequest("GET", path, nil)
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "menu.gohtml", buildMenuPage(r, menu, table)); err != nil {
		t.Fatalf("execute menu.gohtml: %v", err)
	}
	return buf.String()
}

func TestMenuPageRenders(t *testing.T) {
	menu := demoMenu()
	html := renderPageFor(t, "/m/choyxona", menu, &domain.PublicTable{ID: "t1", Label: "Terrace 1"})

	// A template abort leaves a truncated document, so check the tail first —
	// this is the assertion that would have caught the LocaleURL func-field bug.
	if !strings.HasSuffix(strings.TrimSpace(html), "</html>") {
		t.Fatalf("page is truncated; template aborted mid-render:\n%s", html)
	}

	want := map[string]string{
		"restaurant name":     "Choyxona Navruz",
		"address":             "Amir Temur ko&#39;chasi 15",
		"phone link":          `href="tel:`,
		"phone number":        "&#43;998 71 200 00 00",
		"table badge":         "Terrace 1",
		"breakfast menu":      "Nonushta",
		"main menu":           "Asosiy menyu",
		"hot dishes category": "Issiq taomlar",
		"drinks category":     "Ichimliklar",
		"plov":                "Osh",
		"sold-out dish":       "Norin",
		"breakfast item":      "Qatlama",
		"serving window":      "08:00–11:00",
		"weekday restriction": "Mon, Tue, Wed, Thu, Fri",
		"tag":                 "halol",
		"allergen":            "gluten",
		"empty-category note": "Hozircha bo&#39;sh.",
		"sold-out badge":      "tugadi",
		"variant":             "Yarim porsiya",
		"available modifier":  "Qazi",
		"current language":    `aria-current="true" lang="uz"`,
	}
	for what, s := range want {
		if !strings.Contains(html, s) {
			t.Errorf("page is missing the %s (%q)", what, s)
		}
	}

	// Prices must reach the markup, formatted for the currency.
	for _, minor := range []int64{15000, 45000} {
		if formatted := money.Format(minor, "UZS"); !strings.Contains(html, formatted) {
			t.Errorf("page is missing price %q", formatted)
		}
	}

	// A sold-out item shows the badge instead of a price.
	if strings.Contains(html, money.Format(40000, "UZS")) {
		t.Error("sold-out item should not show a price")
	}

	// An unavailable modifier is not offered to the guest.
	if strings.Contains(html, "Bedana tuxumi") {
		t.Error("unavailable modifier should be hidden")
	}
}

func TestMenuPageLanguageSwitcher(t *testing.T) {
	html := renderPageFor(t, "/m/choyxona", demoMenu(), nil)

	for _, locale := range []string{"uz", "ru", "en"} {
		if !strings.Contains(html, `href="/m/choyxona?locale=`+locale+`"`) {
			t.Errorf("language switcher is missing a link to %q", locale)
		}
	}

	// Exactly one entry is marked current. The lang attribute is part of the
	// match so the `a[aria-current="true"]` CSS selector is not counted.
	if n := strings.Count(html, `aria-current="true" lang=`); n != 1 {
		t.Errorf("aria-current appears on %d links, want exactly 1", n)
	}
}

func TestMenuPageSingleLocaleHidesSwitcher(t *testing.T) {
	menu := demoMenu()
	menu.Restaurant.Locales = []string{"uz"}

	if html := renderPageFor(t, "/m/choyxona", menu, nil); strings.Contains(html, `class="langs"`) {
		t.Error("a one-language restaurant should not get a language switcher")
	}
}

func TestMenuPageEmpty(t *testing.T) {
	menu := demoMenu()
	menu.Menus = nil

	html := renderPageFor(t, "/m/choyxona", menu, nil)
	if !strings.Contains(html, "Menyu hali chop etilmagan.") {
		t.Error("an empty restaurant should say so rather than render a blank page")
	}
}

// Locale drives the page's own words, not just the restaurant's content.
func TestMenuPageLabelsFollowLocale(t *testing.T) {
	tests := []struct {
		locale  string
		soldOut string
	}{
		{"uz", "tugadi"},
		{"ru", "нет в наличии"},
		{"en", "sold out"},
		{"de", "sold out"}, // unknown locale falls back to English labels
	}

	for _, tt := range tests {
		t.Run(tt.locale, func(t *testing.T) {
			menu := demoMenu()
			menu.Locale = tt.locale

			html := renderPageFor(t, "/m/choyxona", menu, nil)
			if !strings.Contains(html, tt.soldOut) {
				t.Errorf("locale %q: page is missing %q", tt.locale, tt.soldOut)
			}
			if !strings.Contains(html, `<html lang="`+tt.locale+`">`) {
				t.Errorf("locale %q: html lang attribute is wrong", tt.locale)
			}
		})
	}
}

// An unknown timezone must not take the page down; it falls back to UTC.
func TestMenuPageBadTimezone(t *testing.T) {
	menu := demoMenu()
	menu.Restaurant.Timezone = "Mars/Olympus_Mons"

	renderPageFor(t, "/m/choyxona", menu, nil) // fails the test if it errors
}

func TestNotFoundPageIsWellFormed(t *testing.T) {
	if !strings.HasSuffix(notFoundPage, "</html>") {
		t.Error("not-found page is truncated")
	}
}

func TestRequestedLocale(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		acceptLang string
		want       string
	}{
		{"query wins over header", "/m/x?locale=ru", "en-US,en;q=0.9", "ru"},
		{"query is lowercased", "/m/x?locale=RU", "", "ru"},
		{"query is trimmed", "/m/x?locale=%20ru%20", "", "ru"},
		{"header first preference", "/m/x", "ru-RU,ru;q=0.9,en;q=0.8", "ru"},
		{"header region is stripped", "/m/x", "en-GB", "en"},
		{"header quality is stripped", "/m/x", "uz;q=0.8", "uz"},
		{"nothing requested", "/m/x", "", ""},
		{"empty query falls through to header", "/m/x?locale=", "ru", "ru"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.path, nil)
			if tt.acceptLang != "" {
				r.Header.Set("Accept-Language", tt.acceptLang)
			}
			if got := requestedLocale(r); got != tt.want {
				t.Errorf("requestedLocale() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchesETag(t *testing.T) {
	const etag = `"json-abc-v7-uz-"`

	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"absent", "", false},
		{"exact", etag, true},
		{"wildcard", "*", true},
		{"weak validator", `W/` + etag, true},
		{"in a list", `"other", ` + etag, true},
		{"different version", `"json-abc-v6-uz-"`, false},
		{"different locale", `"json-abc-v7-ru-"`, false},
		{"substring is not a match", `"json-abc-v7-uz-x"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesETag(tt.header, etag); got != tt.want {
				t.Errorf("matchesETag(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

// The ETag must change whenever anything about the rendering changes,
// otherwise a guest keeps a stale menu until the max-age expires.
func TestMenuETagIsDistinguishing(t *testing.T) {
	base := demoMenu()
	tag := menuETag("json", base, nil)

	bumped := demoMenu()
	bumped.Restaurant.MenuVersion++

	other := demoMenu()
	other.Locale = "ru"

	if menuETag("json", bumped, nil) == tag {
		t.Error("a menu version bump must change the ETag")
	}
	if menuETag("json", other, nil) == tag {
		t.Error("a different locale must change the ETag")
	}
	if menuETag("json", base, &domain.PublicTable{ID: "t1"}) == tag {
		t.Error("table context must change the ETag")
	}
	if menuETag("html", base, nil) == tag {
		t.Error("a different representation must change the ETag")
	}
}

func TestWindowLabel(t *testing.T) {
	tests := []struct {
		name string
		menu domain.PublicMenuGroup
		want string
	}{
		{"always on", domain.PublicMenuGroup{}, ""},
		{
			"hours only",
			domain.PublicMenuGroup{AvailableFromMin: ptr(int32(480)), AvailableToMin: ptr(int32(660))},
			"08:00–11:00",
		},
		{
			"hours and days",
			domain.PublicMenuGroup{
				AvailableFromMin: ptr(int32(1320)), AvailableToMin: ptr(int32(120)),
				Days: []int16{5, 6},
			},
			"22:00–02:00 · Fri, Sat",
		},
		{"days only", domain.PublicMenuGroup{Days: []int16{7}}, "Sun"},
		{"out-of-range days are dropped", domain.PublicMenuGroup{Days: []int16{0, 9}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowLabel(tt.menu); got != tt.want {
				t.Errorf("windowLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVariantLine(t *testing.T) {
	got := variantLine([]domain.PublicVariant{
		{Name: "Half", PriceDeltaMinor: -12000, IsAvailable: true},
		{Name: "Full", IsAvailable: true},
		{Name: "Family", PriceDeltaMinor: 20000, IsAvailable: true},
		{Name: "Party", PriceDeltaMinor: 50000},
	}, "UZS")

	// A zero delta prints the name alone; a sign is always shown otherwise.
	for _, want := range []string{"Half " + money.Format(-12000, "UZS"), "Full ·", "Family +", "Party", " — "} {
		if !strings.Contains(got+" ", want) {
			t.Errorf("variantLine() = %q, missing %q", got, want)
		}
	}
}

func TestModifierGroupViewsDropsEmptyGroups(t *testing.T) {
	// A group whose every option is unavailable would render as a bare label
	// with nothing after it, so it is dropped entirely.
	got := modifierGroupViews([]domain.PublicModifierGroup{
		{Name: "Extras", Modifiers: []domain.PublicModifier{{Name: "Qazi"}}},
		{Name: "Sauces", Modifiers: []domain.PublicModifier{{Name: "Chilli", IsAvailable: true}}},
	}, "UZS")

	if len(got) != 1 || got[0].Name != "Sauces" {
		t.Fatalf("modifierGroupViews() = %+v, want only the Sauces group", got)
	}
}

func TestAtoiDefault(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 512},
		{"256", 256},
		{"not-a-number", 512},
		{"-1", -1}, // clamping is qr.ClampSize's job, not this helper's
	}

	for _, tt := range tests {
		if got := atoiDefault(tt.in, 512); got != tt.want {
			t.Errorf("atoiDefault(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
