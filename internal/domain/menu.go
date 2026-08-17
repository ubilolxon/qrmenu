package domain

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/i18n"
)

// MinutesPerDay bounds an availability time, expressed as minutes from midnight
// in the restaurant's local timezone.
const MinutesPerDay = 24 * 60

// MaxPriceMinor guards against a typo turning 30 000 into 30 000 000. It is
// deliberately generous — UZS prices are large numbers — while still catching a
// stray keypress before it reaches a customer's screen.
const MaxPriceMinor int64 = 100_000_000_000

const (
	MaxTags        = 20
	MaxAllergens   = 20
	MaxTagLen      = 40
	MaxSortOrder   = 1_000_000
	MaxReorderSize = 500
)

type Menu struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Name         i18n.Text
	Description  i18n.Text
	IsActive     bool
	SortOrder    int32

	// Both nil means always available.
	AvailableFromMin *int32
	AvailableToMin   *int32
	// ISO day numbers, Monday = 1 .. Sunday = 7. Empty means every day.
	Days []int16

	CreatedAt time.Time
	UpdatedAt time.Time
}

// AvailableAt reports whether the menu is served at the given local time.
//
// A window is allowed to wrap midnight — a late menu running 22:00 to 02:00 has
// from > to — so the comparison is a union of two ranges rather than a simple
// between. The day is taken from when the window *starts*, which is what
// "Friday late menu" means to the person who configured it.
func (m Menu) AvailableAt(local time.Time) bool {
	if !m.IsActive {
		return false
	}

	minutes := int32(local.Hour()*60 + local.Minute())
	isoDay := int16(local.Weekday())
	if isoDay == 0 {
		isoDay = 7 // Go puts Sunday at 0; ISO puts it at 7.
	}

	if m.AvailableFromMin == nil || m.AvailableToMin == nil {
		return len(m.Days) == 0 || slices.Contains(m.Days, isoDay)
	}

	from, to := *m.AvailableFromMin, *m.AvailableToMin

	if from <= to {
		if minutes < from || minutes >= to {
			return false
		}
		return len(m.Days) == 0 || slices.Contains(m.Days, isoDay)
	}

	// Wrapping window. Before `to` belongs to the previous day's window, so the
	// day check uses that day.
	if minutes >= from {
		return len(m.Days) == 0 || slices.Contains(m.Days, isoDay)
	}
	if minutes < to {
		previous := isoDay - 1
		if previous == 0 {
			previous = 7
		}
		return len(m.Days) == 0 || slices.Contains(m.Days, previous)
	}
	return false
}

type Category struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	MenuID       uuid.UUID
	Name         i18n.Text
	Description  i18n.Text
	IsActive     bool
	SortOrder    int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Item struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	CategoryID   uuid.UUID
	Name         i18n.Text
	Description  i18n.Text
	PriceMinor   int64
	IsAvailable  bool
	SortOrder    int32
	Tags         []string
	Allergens    []string
	Calories     *int32
	PrepMinutes  *int32
	ImageID      *uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Variant struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	ItemID          uuid.UUID
	Name            i18n.Text
	PriceDeltaMinor int64
	IsDefault       bool
	IsAvailable     bool
	SortOrder       int32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type ModifierGroup struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	ItemID       uuid.UUID
	Name         i18n.Text
	MinSelect    int32
	MaxSelect    int32
	SortOrder    int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Modifier struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	GroupID         uuid.UUID
	Name            i18n.Text
	PriceDeltaMinor int64
	IsAvailable     bool
	SortOrder       int32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// tagPattern keeps tags machine-usable: they become filter chips and CSS
// classes on the public menu, so arbitrary punctuation causes trouble later.
var tagPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// NormalizeTags lowercases, trims, drops blanks, and removes duplicates while
// preserving the order the owner chose.
func NormalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func validateTagList(v *Validator, field string, tags []string, max int) {
	if len(tags) > max {
		v.Add(field, "must have at most %d entries", max)
		return
	}
	for _, t := range tags {
		if len(t) > MaxTagLen {
			v.Add(field, "%q is longer than %d characters", t, MaxTagLen)
			continue
		}
		if !tagPattern.MatchString(t) {
			v.Add(field, "%q may contain only lowercase letters, digits and single hyphens", t)
		}
	}
}

func validatePrice(v *Validator, field string, minor int64, allowNegative bool) {
	if !allowNegative && minor < 0 {
		v.Add(field, "must not be negative")
		return
	}
	if minor > MaxPriceMinor || minor < -MaxPriceMinor {
		v.Add(field, "is out of range; prices are in minor units, so check for a misplaced digit")
	}
}

func validateSortOrder(v *Validator, order int32) {
	if order < 0 || order > MaxSortOrder {
		v.Add("sort_order", "must be between 0 and %d", MaxSortOrder)
	}
}

// MenuInput is a complete, validated menu.
type MenuInput struct {
	Name             i18n.Text
	Description      i18n.Text
	SortOrder        int32
	AvailableFromMin *int32
	AvailableToMin   *int32
	Days             []int16
}

func (in MenuInput) Validate() error {
	var v Validator
	ValidateTranslated(&v, "name", in.Name, true)
	ValidateTranslated(&v, "description", in.Description, false)
	validateSortOrder(&v, in.SortOrder)
	ValidateAvailability(&v, in.AvailableFromMin, in.AvailableToMin, in.Days)
	return v.Err()
}

// ValidateAvailability checks the serving window and day list.
func ValidateAvailability(v *Validator, from, to *int32, days []int16) {
	// The database enforces this too; catching it here produces a message that
	// names the field instead of a constraint.
	if (from == nil) != (to == nil) {
		v.Add("available_from_min", "must be set together with available_to_min, or both left empty")
	}
	if from != nil && (*from < 0 || *from >= MinutesPerDay) {
		v.Add("available_from_min", "must be between 0 and %d", MinutesPerDay-1)
	}
	if to != nil && (*to < 0 || *to >= MinutesPerDay) {
		v.Add("available_to_min", "must be between 0 and %d", MinutesPerDay-1)
	}
	// Equal endpoints describe either a zero-length or a 24-hour window; both
	// readings are defensible, so require the owner to say which they meant.
	if from != nil && to != nil && *from == *to {
		v.Add("available_to_min", "must differ from available_from_min; leave both empty for always available")
	}

	if len(days) > 7 {
		v.Add("days", "must have at most 7 entries")
		return
	}
	seen := make(map[int16]bool, len(days))
	for _, d := range days {
		if d < 1 || d > 7 {
			v.Add("days", "%d is not a valid ISO weekday; use 1 for Monday through 7 for Sunday", d)
			continue
		}
		if seen[d] {
			v.Add("days", "%d is listed twice", d)
		}
		seen[d] = true
	}
}

type CategoryInput struct {
	MenuID      uuid.UUID
	Name        i18n.Text
	Description i18n.Text
	SortOrder   int32
}

func (in CategoryInput) Validate() error {
	var v Validator
	v.Check(in.MenuID != uuid.Nil, "menu_id", "is required")
	ValidateTranslated(&v, "name", in.Name, true)
	ValidateTranslated(&v, "description", in.Description, false)
	validateSortOrder(&v, in.SortOrder)
	return v.Err()
}

type ItemInput struct {
	CategoryID  uuid.UUID
	Name        i18n.Text
	Description i18n.Text
	PriceMinor  int64
	SortOrder   int32
	Tags        []string
	Allergens   []string
	Calories    *int32
	PrepMinutes *int32
}

func (in ItemInput) Validate() error {
	var v Validator
	v.Check(in.CategoryID != uuid.Nil, "category_id", "is required")
	ValidateTranslated(&v, "name", in.Name, true)
	ValidateTranslated(&v, "description", in.Description, false)
	validatePrice(&v, "price_minor", in.PriceMinor, false)
	validateSortOrder(&v, in.SortOrder)
	validateTagList(&v, "tags", in.Tags, MaxTags)
	validateTagList(&v, "allergens", in.Allergens, MaxAllergens)

	if in.Calories != nil && (*in.Calories < 0 || *in.Calories > 20000) {
		v.Add("calories", "must be between 0 and 20000")
	}
	if in.PrepMinutes != nil && (*in.PrepMinutes < 0 || *in.PrepMinutes > 1440) {
		v.Add("prep_minutes", "must be between 0 and 1440")
	}
	return v.Err()
}

type VariantInput struct {
	ItemID          uuid.UUID
	Name            i18n.Text
	PriceDeltaMinor int64
	IsDefault       bool
	SortOrder       int32
}

func (in VariantInput) Validate() error {
	var v Validator
	v.Check(in.ItemID != uuid.Nil, "item_id", "is required")
	ValidateTranslated(&v, "name", in.Name, true)
	// A smaller size may legitimately cost less than the base price.
	validatePrice(&v, "price_delta_minor", in.PriceDeltaMinor, true)
	validateSortOrder(&v, in.SortOrder)
	return v.Err()
}

type ModifierGroupInput struct {
	ItemID    uuid.UUID
	Name      i18n.Text
	MinSelect int32
	MaxSelect int32
	SortOrder int32
}

func (in ModifierGroupInput) Validate() error {
	var v Validator
	v.Check(in.ItemID != uuid.Nil, "item_id", "is required")
	ValidateTranslated(&v, "name", in.Name, true)
	validateSortOrder(&v, in.SortOrder)

	if in.MinSelect < 0 {
		v.Add("min_select", "must not be negative")
	}
	if in.MaxSelect < 1 {
		v.Add("max_select", "must be at least 1")
	}
	if in.MinSelect > in.MaxSelect {
		v.Add("min_select", "must not be greater than max_select")
	}
	return v.Err()
}

type ModifierInput struct {
	GroupID         uuid.UUID
	Name            i18n.Text
	PriceDeltaMinor int64
	SortOrder       int32
}

func (in ModifierInput) Validate() error {
	var v Validator
	v.Check(in.GroupID != uuid.Nil, "group_id", "is required")
	ValidateTranslated(&v, "name", in.Name, true)
	validatePrice(&v, "price_delta_minor", in.PriceDeltaMinor, true)
	validateSortOrder(&v, in.SortOrder)
	return v.Err()
}

// ValidateReorder checks a bulk reorder payload.
func ValidateReorder(ids []uuid.UUID) error {
	var v Validator
	switch {
	case len(ids) == 0:
		v.Add("ids", "must list at least one id")
	case len(ids) > MaxReorderSize:
		v.Add("ids", "must list at most %d ids", MaxReorderSize)
	}

	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			v.Add("ids", "%s is listed twice", id)
		}
		seen[id] = true
	}
	return v.Err()
}
