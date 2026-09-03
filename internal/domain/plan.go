package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/i18n"
)

// A plan is what a tenant is allowed to do; a subscription is the state of
// their agreement to pay for it. Both live here rather than in the service
// layer because every rule below — what a limit means, which status may follow
// which, whether a suspended account may still write — is a business decision
// that should be readable and testable without a database.

// Unlimited is the sentinel a limit carries when a plan does not cap the
// resource at all. Zero is a real value meaning "none allowed", so "no limit"
// needs a value of its own rather than an absent one.
const Unlimited = -1

// Resource names a thing a plan puts a ceiling on. The string values match the
// jsonb keys in plans.limits, so a new lever is a migration and a constant.
type Resource string

const (
	ResourceRestaurants Resource = "restaurants"
	ResourceMenuItems   Resource = "menu_items"
	ResourceTables      Resource = "tables"
	ResourceStaffUsers  Resource = "staff_users"
	ResourceLocales     Resource = "locales"
	ResourceStorageMB   Resource = "storage_mb"
)

// Limits is the decoded plans.limits document.
//
// Every field is decoded from an explicit key, and a key that is absent decodes
// to zero — "not allowed" — rather than to unlimited. That way adding a lever
// does not silently hand it to every plan already in the database; the
// migration that adds it also has to grant it.
type Limits struct {
	Restaurants int `json:"restaurants"`
	MenuItems   int `json:"menu_items"`
	Tables      int `json:"tables"`
	StaffUsers  int `json:"staff_users"`
	Locales     int `json:"locales"`
	StorageMB   int `json:"storage_mb"`

	// Feature flags rather than counts. OrdersEnabled gates the guest ordering
	// flow; the menu itself is never gated, because a QR code already printed
	// and stuck to a table has to keep working whatever the account is doing.
	OrdersEnabled   bool `json:"orders_enabled"`
	CustomBrand     bool `json:"custom_brand"`
	RemoveWatermark bool `json:"remove_watermark"`
}

// Cap reports the ceiling for one resource, or Unlimited.
func (l Limits) Cap(r Resource) int {
	switch r {
	case ResourceRestaurants:
		return l.Restaurants
	case ResourceMenuItems:
		return l.MenuItems
	case ResourceTables:
		return l.Tables
	case ResourceStaffUsers:
		return l.StaffUsers
	case ResourceLocales:
		return l.Locales
	case ResourceStorageMB:
		return l.StorageMB
	default:
		// An unknown resource is a programming error, and answering "unlimited"
		// would turn it into a silently unenforced limit. Refuse instead: the
		// first call site to use a name that does not exist fails loudly.
		return 0
	}
}

// Room reports how many more of a resource may be added, and whether the answer
// is meaningful — false means unlimited, so there is no number to show.
func (l Limits) Room(r Resource, current int) (int, bool) {
	ceiling := l.Cap(r)
	if ceiling == Unlimited {
		return 0, false
	}
	if current >= ceiling {
		return 0, true
	}
	return ceiling - current, true
}

func ParseLimits(raw []byte) (Limits, error) {
	var l Limits
	if len(raw) == 0 {
		return l, nil
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return Limits{}, fmt.Errorf("decode plan limits: %w", err)
	}
	return l, nil
}

// ------------------------------------------------------------------- errors

// LimitError is what a write refused by a plan returns. It carries the numbers
// rather than a rendered sentence, so the SPA can put "3 of 3 restaurants used"
// next to an upgrade button instead of parsing a message.
type LimitError struct {
	Resource Resource `json:"resource"`
	Limit    int      `json:"limit"`
	Current  int      `json:"current"`
	PlanCode string   `json:"plan_code"`
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("plan %s allows %d %s, %d in use", e.PlanCode, e.Limit, e.Resource, e.Current)
}

func (e *LimitError) Unwrap() error { return ErrPlanLimit }

// Message is the user-facing sentence. It names the plan and the number,
// because "upgrade your plan" without either is a dead end.
func (e *LimitError) Message() string {
	noun := strings.ReplaceAll(string(e.Resource), "_", " ")
	if e.Limit == 0 {
		return fmt.Sprintf("Your %s plan does not include %s. Upgrade to add them.", e.PlanCode, noun)
	}
	// "allows 1 restaurants" reads like a bug in the sentence rather than a
	// limit, and this is the number a customer sees most often — every plan has
	// something set to one.
	if e.Limit == 1 {
		return fmt.Sprintf("Your %s plan allows one %s. Upgrade to add more.",
			e.PlanCode, singular(noun))
	}
	return fmt.Sprintf("Your %s plan allows %d %s and you are using %d. Upgrade to add more.",
		e.PlanCode, e.Limit, noun, e.Current)
}

// singular trims the plural s off a resource name. Every resource is a plain
// English plural — restaurants, tables, locales — so nothing more elaborate
// than this is warranted, and a name that is not simply keeps its s.
func singular(noun string) string {
	return strings.TrimSuffix(noun, "s")
}

// FeatureError is the flag equivalent of LimitError: the plan does not include
// a capability at all, so there is no count to report.
type FeatureError struct {
	Feature  string `json:"feature"`
	PlanCode string `json:"plan_code"`
}

func (e *FeatureError) Error() string {
	return fmt.Sprintf("plan %s does not include %s", e.PlanCode, e.Feature)
}

func (e *FeatureError) Unwrap() error { return ErrPlanLimit }

func (e *FeatureError) Message() string {
	return fmt.Sprintf("Your %s plan does not include %s. Upgrade to enable it.", e.PlanCode, e.Feature)
}

// ------------------------------------------------------------------- plans

type BillingInterval string

const (
	IntervalMonth BillingInterval = "month"
	IntervalYear  BillingInterval = "year"
)

// Months is how far one paid period runs. Renewal adds this to the current
// period end rather than to now, so a payment that arrives late does not
// shorten the month the customer bought.
func (b BillingInterval) Months() int {
	if b == IntervalYear {
		return 12
	}
	return 1
}

type Plan struct {
	Code            string          `json:"code"`
	Name            i18n.Text       `json:"name"`
	Description     i18n.Text       `json:"description,omitempty"`
	PriceMinor      int64           `json:"price_minor"`
	Currency        string          `json:"currency"`
	BillingInterval BillingInterval `json:"billing_interval"`
	TrialDays       int             `json:"trial_days"`
	Limits          Limits          `json:"limits"`
	IsPublic        bool            `json:"is_public"`
	SortOrder       int             `json:"sort_order"`
}

// FreePlanCode is the plan a new tenant starts on and the one a cancelled or
// expired subscription falls back to. It exists in the database from the
// billing migration onward, because a tenant with no plan has no valid state.
const FreePlanCode = "free"

// --------------------------------------------------------------- subscription

type SubscriptionStatus string

const (
	// Trialing and Active are indistinguishable in what they permit; they
	// differ only in whether money has changed hands, which matters to a
	// dunning email and to nothing else.
	SubTrialing  SubscriptionStatus = "trialing"
	SubActive    SubscriptionStatus = "active"
	SubPastDue   SubscriptionStatus = "past_due"
	SubSuspended SubscriptionStatus = "suspended"
	SubCancelled SubscriptionStatus = "cancelled"
)

var subscriptionTransitions = map[SubscriptionStatus][]SubscriptionStatus{
	SubTrialing:  {SubActive, SubPastDue, SubCancelled},
	SubActive:    {SubPastDue, SubCancelled},
	SubPastDue:   {SubActive, SubSuspended, SubCancelled},
	SubSuspended: {SubActive, SubCancelled},
	SubCancelled: nil,
}

func (s SubscriptionStatus) Valid() bool {
	_, ok := subscriptionTransitions[s]
	return ok
}

func (s SubscriptionStatus) CanTransition(to SubscriptionStatus) bool {
	for _, allowed := range subscriptionTransitions[s] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Writable reports whether the tenant may still create things.
//
// past_due deliberately stays writable: it is the grace window, and locking an
// account the moment a card expires punishes the restaurant for a bank's
// timing. Suspension is the lever, and it comes after the grace period.
func (s SubscriptionStatus) Writable() bool {
	return s == SubTrialing || s == SubActive || s == SubPastDue
}

type Subscription struct {
	ID                 uuid.UUID          `json:"id"`
	TenantID           uuid.UUID          `json:"-"`
	PlanCode           string             `json:"plan_code"`
	Status             SubscriptionStatus `json:"status"`
	Provider           PaymentProvider    `json:"provider"`
	ProviderRef        *string            `json:"-"`
	CurrentPeriodStart time.Time          `json:"current_period_start"`
	CurrentPeriodEnd   *time.Time         `json:"current_period_end"`
	CancelAtPeriodEnd  bool               `json:"cancel_at_period_end"`
	CancelledAt        *time.Time         `json:"cancelled_at"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// Expired reports whether the paid period has run out. A subscription with no
// end date never expires, which is what a free plan is.
func (s Subscription) Expired(now time.Time) bool {
	return s.CurrentPeriodEnd != nil && s.CurrentPeriodEnd.Before(now)
}

type PaymentProvider string

const (
	// ProviderManual is an admin marking an invoice paid — a bank transfer, a
	// cash payment, a comped account. It ships in the MVP and stays afterwards,
	// because every payment system eventually needs a human override.
	ProviderManual PaymentProvider = "manual"
	ProviderPayme  PaymentProvider = "payme"
	ProviderClick  PaymentProvider = "click"
	ProviderStripe PaymentProvider = "stripe"
)

func (p PaymentProvider) Valid() bool {
	switch p {
	case ProviderManual, ProviderPayme, ProviderClick, ProviderStripe:
		return true
	}
	return false
}

// -------------------------------------------------------------- entitlement

// Entitlement is the answer to "what is this tenant allowed to do right now" —
// the plan's limits joined to the subscription's status. It is resolved once
// per write and then asked several questions, so the checks below take the
// current usage as an argument rather than counting anything themselves.
type Entitlement struct {
	Subscription Subscription    `json:"subscription"`
	PlanCode     string          `json:"plan_code"`
	PlanName     i18n.Text       `json:"plan_name"`
	PriceMinor   int64           `json:"price_minor"`
	Currency     string          `json:"currency"`
	Interval     BillingInterval `json:"billing_interval"`
	Limits       Limits          `json:"limits"`
}

// Check reports whether one more of a resource may be created.
//
// current is what exists now, so the test is >= rather than >: a tenant holding
// exactly their limit may not add another. Suspension is checked first, because
// a suspended account is refused regardless of how much room its plan has.
func (e Entitlement) Check(r Resource, current int) error {
	if !e.Subscription.Status.Writable() {
		return &FeatureError{Feature: "changes while the subscription is " + string(e.Subscription.Status), PlanCode: e.PlanCode}
	}
	limit := e.Limits.Cap(r)
	if limit == Unlimited {
		return nil
	}
	if current >= limit {
		return &LimitError{Resource: r, Limit: limit, Current: current, PlanCode: e.PlanCode}
	}
	return nil
}

// CheckAtLeast is the variant for a value that is set outright rather than
// incremented — the number of locales on a restaurant, say, where a PATCH
// replaces the whole list instead of adding one.
func (e Entitlement) CheckAtLeast(r Resource, wanted int) error {
	if !e.Subscription.Status.Writable() {
		return &FeatureError{Feature: "changes while the subscription is " + string(e.Subscription.Status), PlanCode: e.PlanCode}
	}
	limit := e.Limits.Cap(r)
	if limit == Unlimited || wanted <= limit {
		return nil
	}
	return &LimitError{Resource: r, Limit: limit, Current: wanted, PlanCode: e.PlanCode}
}

// CheckFeature gates a boolean capability.
func (e Entitlement) CheckFeature(name string, enabled bool) error {
	if !enabled {
		return &FeatureError{Feature: name, PlanCode: e.PlanCode}
	}
	return nil
}
