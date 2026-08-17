package handler

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/money"
	"github.com/ubilolxon/qrmenu/internal/qr"
	"github.com/ubilolxon/qrmenu/internal/service"
)

//go:embed templates/*.gohtml
var templateFS embed.FS

type Public struct {
	svc  *service.PublicMenu
	cfg  config.Public
	tmpl *template.Template
}

func NewPublic(svc *service.PublicMenu, cfg config.Public) (Public, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.gohtml")
	if err != nil {
		return Public{}, fmt.Errorf("parse menu templates: %w", err)
	}
	return Public{svc: svc, cfg: cfg, tmpl: tmpl}, nil
}

// ---------------------------------------------------------------- JSON

type publicMenuResponse struct {
	*domain.PublicMenu
	Table *domain.PublicTable `json:"table,omitempty"`
}

func (h Public) MenuBySlug(w http.ResponseWriter, r *http.Request) {
	menu, err := h.svc.BySlug(r.Context(), chi.URLParam(r, "slug"), requestedLocale(r))
	if err != nil {
		RenderError(w, r, err)
		return
	}
	h.writeMenuJSON(w, r, menu, nil)
}

func (h Public) MenuByToken(w http.ResponseWriter, r *http.Request) {
	menu, table, err := h.svc.ByToken(r.Context(), chi.URLParam(r, "token"), requestedLocale(r))
	if err != nil {
		RenderError(w, r, err)
		return
	}
	h.writeMenuJSON(w, r, menu, table)
}

// writeMenuJSON adds the caching that makes this endpoint survive a dinner rush.
//
// The payload is time-independent, so it can carry a strong ETag built from the
// menu version. A repeat scan at the same table costs a 304 with no body, and a
// CDN in front serves most requests without reaching the origin at all.
func (h Public) writeMenuJSON(w http.ResponseWriter, r *http.Request, menu *domain.PublicMenu, table *domain.PublicTable) {
	etag := menuETag("json", menu, table)

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d",
		int(h.cfg.CacheMaxAge.Seconds()), int(h.cfg.StaleWhileRevalidate.Seconds())))
	// The response differs by language, so a shared cache must key on it too.
	w.Header().Add("Vary", "Accept-Language")

	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	httperr.JSON(w, http.StatusOK, publicMenuResponse{PublicMenu: menu, Table: table})
}

// menuETag identifies one rendering. The menu version changes on every write,
// so the tag changes exactly when the content does.
func menuETag(kind string, menu *domain.PublicMenu, table *domain.PublicTable) string {
	tableID := ""
	if table != nil {
		tableID = table.ID
	}
	return fmt.Sprintf(`"%s-%s-v%d-%s-%s"`,
		kind, menu.Restaurant.ID, menu.Restaurant.MenuVersion, menu.Locale, tableID)
}

// matchesETag implements the If-None-Match comparison, which is a list and may
// contain the wildcard.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		// A weak validator matches the same representation.
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// requestedLocale prefers an explicit ?locale= (the language switcher) and
// otherwise takes the phone's first Accept-Language preference. An unsupported
// value falls back to the restaurant's default rather than failing.
func requestedLocale(r *http.Request) string {
	if l := strings.TrimSpace(r.URL.Query().Get("locale")); l != "" {
		return strings.ToLower(l)
	}

	header := r.Header.Get("Accept-Language")
	if header == "" {
		return ""
	}
	first, _, _ := strings.Cut(header, ",")
	first, _, _ = strings.Cut(first, ";")
	// "ru-RU" should match a restaurant publishing "ru".
	base, _, _ := strings.Cut(strings.TrimSpace(first), "-")
	return strings.ToLower(base)
}

// ---------------------------------------------------------------- HTML

func (h Public) PageBySlug(w http.ResponseWriter, r *http.Request) {
	menu, err := h.svc.BySlug(r.Context(), chi.URLParam(r, "slug"), requestedLocale(r))
	if err != nil {
		h.renderErrorPage(w, r, err)
		return
	}
	h.renderPage(w, r, menu, nil)
}

func (h Public) PageByToken(w http.ResponseWriter, r *http.Request) {
	menu, table, err := h.svc.ByToken(r.Context(), chi.URLParam(r, "token"), requestedLocale(r))
	if err != nil {
		h.renderErrorPage(w, r, err)
		return
	}
	h.renderPage(w, r, menu, table)
}

func (h Public) renderPage(w http.ResponseWriter, r *http.Request, menu *domain.PublicMenu, table *domain.PublicTable) {
	page := buildMenuPage(r, menu, table)

	// Unlike the JSON, this page depends on the current time: which menus are
	// serving right now is baked into the HTML. So it gets a short freshness
	// window and no ETag — a validator derived only from menu_version would
	// let a breakfast menu be revalidated as fresh at dinner time.
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Add("Vary", "Accept-Language")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := h.tmpl.ExecuteTemplate(w, "menu.gohtml", page); err != nil {
		// The status line is already sent by now, so this can only be logged.
		httperr.Internal(w, r, fmt.Errorf("render menu page: %w", err))
	}
}

func (h Public) renderErrorPage(w http.ResponseWriter, r *http.Request, err error) {
	// A guest who scanned a dead code should see words, not JSON.
	if isNotFound(err) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(notFoundPage))
		return
	}
	RenderError(w, r, err)
}

const notFoundPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Menu not found</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;
font:16px/1.6 system-ui,sans-serif;background:#fff;color:#141414;text-align:center;padding:2rem}
@media(prefers-color-scheme:dark){body{background:#131211;color:#f2efea}}
h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#6b6b6b;margin:0}</style></head>
<body><div><h1>Menu not found</h1><p>This code is no longer active. Please ask a member of staff.</p></div></body></html>`

// ---------------------------------------------------------------- QR

// QRBySlug is parameterised by format so each route binds one explicit
// extension, rather than parsing an extension out of the path.
func (h Public) QRBySlug(format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")

		// Confirm the restaurant exists before encoding anything. Without this
		// the endpoint is an open QR generator: anyone could mint codes on this
		// domain pointing at arbitrary paths, which is a ready-made phishing
		// tool that borrows the restaurant's trust.
		menu, err := h.svc.BySlug(r.Context(), slug, "")
		if err != nil {
			RenderError(w, r, err)
			return
		}

		h.writeQR(w, r, format, h.cfg.BaseURL+"/m/"+menu.Restaurant.Slug,
			menu.Restaurant.Name, "", "menu-"+menu.Restaurant.Slug)
	}
}

func (h Public) QRByToken(format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := domain.NormalizeQRToken(chi.URLParam(r, "token"))

		menu, table, err := h.svc.ByToken(r.Context(), token, "")
		if err != nil {
			RenderError(w, r, err)
			return
		}

		h.writeQR(w, r, format, h.cfg.BaseURL+"/t/"+token,
			menu.Restaurant.Name, table.Label, "table-"+strings.ToLower(table.Label))
	}
}

// writeQR renders png, svg or a printable card depending on the extension.
func (h Public) writeQR(w http.ResponseWriter, r *http.Request, format, content, restaurantName, tableLabel, filename string) {
	size := qr.ClampSize(atoiDefault(r.URL.Query().Get("size"), qr.DefaultSize))

	var (
		body  []byte
		ctype string
		err   error
	)
	switch format {
	case "svg":
		body, err = qr.SVG(content, size)
		ctype = "image/svg+xml"
	case "card.svg", "card":
		label := tableLabel
		if label == "" {
			label = "Menu"
		}
		body, err = qr.Card(content, restaurantName, label, size)
		ctype = "image/svg+xml"
		filename += "-card"
	default: // png
		body, err = qr.PNG(content, size)
		ctype = "image/png"
	}
	if err != nil {
		httperr.Internal(w, r, err)
		return
	}

	ext := ".png"
	if strings.HasSuffix(ctype, "svg+xml") {
		ext = ".svg"
	}

	w.Header().Set("Content-Type", ctype)
	// The QR content is fixed for a given URL, so it can be cached hard. It
	// only changes if the table token is rotated, which changes the URL too.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s%s"`, filename, ext))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------------------------------------------------------------- view model

// The template gets a flattened, already-formatted view. Prices are rendered
// here rather than in the template because formatting depends on the currency,
// and availability is computed here because it depends on the restaurant's
// timezone — neither belongs in markup.
type menuPage struct {
	Menu        *domain.PublicMenu
	Table       *domain.PublicTable
	Locale      string
	Menus       []menuSectionView
	T           labels
	LocaleLinks []localeLink
}

// The language switcher is precomputed rather than exposed as a func field:
// html/template invokes methods and FuncMap entries, but not func-typed struct
// fields, and that failure only shows up at render time.
type localeLink struct {
	Locale  string
	URL     string
	Current bool
}

type menuSectionView struct {
	Name         string
	Description  string
	AvailableNow bool
	WindowLabel  string
	Categories   []categoryView
}

type categoryView struct {
	Name  string
	Items []itemView
}

type itemView struct {
	Name           string
	Description    string
	Price          string
	IsAvailable    bool
	Tags           []string
	Allergens      []string
	Variants       []domain.PublicVariant
	VariantLine    string
	ModifierGroups []modifierGroupView
}

type modifierGroupView struct {
	Name string
	Line string
}

func buildMenuPage(r *http.Request, menu *domain.PublicMenu, table *domain.PublicTable) menuPage {
	loc, err := time.LoadLocation(menu.Restaurant.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	currency := menu.Restaurant.Currency

	sections := make([]menuSectionView, 0, len(menu.Menus))
	for _, m := range menu.Menus {
		// Reconstruct just enough of the domain type to reuse the tested
		// availability rules rather than reimplementing them for the template.
		domainMenu := domain.Menu{
			IsActive:         true,
			AvailableFromMin: m.AvailableFromMin,
			AvailableToMin:   m.AvailableToMin,
			Days:             m.Days,
		}

		categories := make([]categoryView, 0, len(m.Categories))
		for _, c := range m.Categories {
			items := make([]itemView, 0, len(c.Items))
			for _, it := range c.Items {
				items = append(items, itemView{
					Name:           it.Name,
					Description:    it.Description,
					Price:          money.Format(it.PriceMinor, currency),
					IsAvailable:    it.IsAvailable,
					Tags:           it.Tags,
					Allergens:      it.Allergens,
					Variants:       it.Variants,
					VariantLine:    variantLine(it.Variants, currency),
					ModifierGroups: modifierGroupViews(it.ModifierGroups, currency),
				})
			}
			categories = append(categories, categoryView{Name: c.Name, Items: items})
		}

		sections = append(sections, menuSectionView{
			Name:         m.Name,
			Description:  m.Description,
			AvailableNow: domainMenu.AvailableAt(now),
			WindowLabel:  windowLabel(m),
			Categories:   categories,
		})
	}

	links := make([]localeLink, 0, len(menu.Restaurant.Locales))
	for _, l := range menu.Restaurant.Locales {
		links = append(links, localeLink{
			Locale:  l,
			URL:     r.URL.Path + "?locale=" + url.QueryEscape(l),
			Current: l == menu.Locale,
		})
	}

	return menuPage{
		Menu:        menu,
		Table:       table,
		Locale:      menu.Locale,
		Menus:       sections,
		T:           labelsFor(menu.Locale),
		LocaleLinks: links,
	}
}

func variantLine(variants []domain.PublicVariant, currency string) string {
	parts := make([]string, 0, len(variants))
	for _, v := range variants {
		part := v.Name
		switch {
		case v.PriceDeltaMinor > 0:
			part += " +" + money.Format(v.PriceDeltaMinor, currency)
		case v.PriceDeltaMinor < 0:
			part += " " + money.Format(v.PriceDeltaMinor, currency)
		}
		if !v.IsAvailable {
			part += " —"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

func modifierGroupViews(groups []domain.PublicModifierGroup, currency string) []modifierGroupView {
	out := make([]modifierGroupView, 0, len(groups))
	for _, g := range groups {
		parts := make([]string, 0, len(g.Modifiers))
		for _, m := range g.Modifiers {
			if !m.IsAvailable {
				continue
			}
			part := m.Name
			if m.PriceDeltaMinor != 0 {
				part += " +" + money.Format(m.PriceDeltaMinor, currency)
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, modifierGroupView{Name: g.Name, Line: strings.Join(parts, " · ")})
	}
	return out
}

// windowLabel renders "08:00–11:00" plus weekdays when the menu is restricted.
func windowLabel(m domain.PublicMenuGroup) string {
	var parts []string
	if m.AvailableFromMin != nil && m.AvailableToMin != nil {
		parts = append(parts, fmt.Sprintf("%s–%s", hhmm(*m.AvailableFromMin), hhmm(*m.AvailableToMin)))
	}
	if len(m.Days) > 0 {
		names := make([]string, 0, len(m.Days))
		for _, d := range m.Days {
			if d >= 1 && d <= 7 {
				names = append(names, weekdayNames[d])
			}
		}
		parts = append(parts, strings.Join(names, ", "))
	}
	return strings.Join(parts, " · ")
}

var weekdayNames = map[int16]string{
	1: "Mon", 2: "Tue", 3: "Wed", 4: "Thu", 5: "Fri", 6: "Sat", 7: "Sun",
}

func hhmm(minutes int32) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// labels are the handful of words the page itself contributes, as opposed to
// the restaurant's own content. A full i18n framework would be overkill for six
// strings; this is enough until the SPA takes over.
type labels struct {
	Table   string
	SoldOut string
	Sizes   string
	NoMenu  string
	NoItems string
	Prices  string
}

func labelsFor(locale string) labels {
	switch locale {
	case "ru":
		return labels{
			Table: "Стол", SoldOut: "нет в наличии", Sizes: "Размеры:",
			NoMenu: "Меню пока не опубликовано.", NoItems: "Пока пусто.",
			Prices: "Цены в",
		}
	case "uz":
		return labels{
			Table: "Stol", SoldOut: "tugadi", Sizes: "O'lchamlar:",
			NoMenu: "Menyu hali chop etilmagan.", NoItems: "Hozircha bo'sh.",
			Prices: "Narxlar",
		}
	default:
		return labels{
			Table: "Table", SoldOut: "sold out", Sizes: "Sizes:",
			NoMenu: "This menu has not been published yet.", NoItems: "Nothing here yet.",
			Prices: "Prices in",
		}
	}
}
