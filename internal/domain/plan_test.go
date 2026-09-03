package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubilolxon/qrmenu/internal/domain"
)

// The whole point of keeping limits in domain is that these rules can be tested
// without a database, a plan table, or a subscription. Everything below is a
// pure function of a Limits value and a count.

func entitlement(plan string, status domain.SubscriptionStatus, limits domain.Limits) domain.Entitlement {
	return domain.Entitlement{
		Subscription: domain.Subscription{Status: status},
		PlanCode:     plan,
		Limits:       limits,
	}
}

// A missing key must read as "not allowed", never as "unlimited". Getting this
// backwards would mean that adding a new lever silently grants it to every plan
// already in the database.
func TestAbsentLimitIsZeroNotUnlimited(t *testing.T) {
	limits, err := domain.ParseLimits([]byte(`{"restaurants": 3}`))
	if err != nil {
		t.Fatalf("parse limits: %v", err)
	}

	if got := limits.Cap(domain.ResourceRestaurants); got != 3 {
		t.Errorf("restaurants cap = %d, want 3", got)
	}
	if got := limits.Cap(domain.ResourceTables); got != 0 {
		t.Errorf("absent tables cap = %d, want 0", got)
	}
	if limits.OrdersEnabled {
		t.Error("absent orders_enabled decoded as true")
	}
}

func TestUnknownResourceIsRefusedNotWaved(t *testing.T) {
	limits := domain.Limits{Restaurants: domain.Unlimited}
	if got := limits.Cap(domain.Resource("wormholes")); got != 0 {
		t.Errorf("unknown resource cap = %d, want 0 — an unknown name must refuse, not permit", got)
	}
}

func TestCheckIsInclusiveOfTheLimit(t *testing.T) {
	ent := entitlement("basic", domain.SubActive, domain.Limits{Restaurants: 3})

	if err := ent.Check(domain.ResourceRestaurants, 2); err != nil {
		t.Errorf("holding 2 of 3 should allow a third: %v", err)
	}
	// The boundary that matters: a tenant holding exactly their limit may not
	// add another. An off-by-one here gives every plan one extra of everything.
	err := ent.Check(domain.ResourceRestaurants, 3)
	if err == nil {
		t.Fatal("holding 3 of 3 should refuse a fourth")
	}
	if !errors.Is(err, domain.ErrPlanLimit) {
		t.Errorf("error does not unwrap to ErrPlanLimit: %v", err)
	}

	var limitErr *domain.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("want a *LimitError, got %T", err)
	}
	if limitErr.Limit != 3 || limitErr.Current != 3 || limitErr.PlanCode != "basic" {
		t.Errorf("limit error = %+v, want limit 3, current 3, plan basic", limitErr)
	}
}

func TestUnlimitedNeverRefuses(t *testing.T) {
	ent := entitlement("pro", domain.SubActive, domain.Limits{MenuItems: domain.Unlimited})
	if err := ent.Check(domain.ResourceMenuItems, 1_000_000); err != nil {
		t.Errorf("unlimited plan refused at a million items: %v", err)
	}
	if _, known := ent.Limits.Room(domain.ResourceMenuItems, 5); known {
		t.Error("Room reported a number for an unlimited resource")
	}
}

func TestZeroLimitMeansNoneAllowed(t *testing.T) {
	ent := entitlement("free", domain.SubActive, domain.Limits{Tables: 0})
	err := ent.Check(domain.ResourceTables, 0)
	if err == nil {
		t.Fatal("a limit of zero should refuse the first one")
	}

	var limitErr *domain.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("want a *LimitError, got %T", err)
	}
	// The message has to say something a customer can act on rather than
	// "0 of 0", which reads like a bug.
	if got := limitErr.Message(); !strings.Contains(got, "does not include") {
		t.Errorf("message = %q, want it to say the plan does not include them", got)
	}
}

// A suspended account is refused whatever room its plan has. This is the
// difference between "you have run out" and "you have stopped paying".
func TestSuspendedAccountCannotWriteEvenWithRoom(t *testing.T) {
	ent := entitlement("pro", domain.SubSuspended, domain.Limits{Restaurants: domain.Unlimited})

	err := ent.Check(domain.ResourceRestaurants, 0)
	if err == nil {
		t.Fatal("a suspended subscription should refuse a write")
	}
	if !errors.Is(err, domain.ErrPlanLimit) {
		t.Errorf("error does not unwrap to ErrPlanLimit: %v", err)
	}
}

// past_due is the grace window and stays writable: locking an account the
// moment a card expires punishes the restaurant for a bank's timing.
func TestPastDueStaysWritable(t *testing.T) {
	ent := entitlement("basic", domain.SubPastDue, domain.Limits{Restaurants: 2})
	if err := ent.Check(domain.ResourceRestaurants, 1); err != nil {
		t.Errorf("past_due should still allow writes inside the plan: %v", err)
	}
}

func TestCheckAtLeastComparesTheWholeValue(t *testing.T) {
	ent := entitlement("basic", domain.SubActive, domain.Limits{Locales: 3})

	if err := ent.CheckAtLeast(domain.ResourceLocales, 3); err != nil {
		t.Errorf("exactly the limit should be allowed when the value is set outright: %v", err)
	}
	if err := ent.CheckAtLeast(domain.ResourceLocales, 4); err == nil {
		t.Error("one over the limit should be refused")
	}
}

func TestFeatureRefusalCarriesThePlan(t *testing.T) {
	ent := entitlement("free", domain.SubActive, domain.Limits{OrdersEnabled: false})

	err := ent.CheckFeature("table ordering", ent.Limits.OrdersEnabled)
	if err == nil {
		t.Fatal("a plan without ordering should refuse it")
	}
	var featureErr *domain.FeatureError
	if !errors.As(err, &featureErr) {
		t.Fatalf("want a *FeatureError, got %T", err)
	}
	if featureErr.PlanCode != "free" || featureErr.Feature != "table ordering" {
		t.Errorf("feature error = %+v, want free / table ordering", featureErr)
	}

	if err := ent.CheckFeature("table ordering", true); err != nil {
		t.Errorf("an enabled feature should pass: %v", err)
	}
}

// The refusal is serialised into the error envelope, so the SPA can render
// "3 of 3 used" without parsing a sentence.
func TestLimitErrorSerialisesItsNumbers(t *testing.T) {
	raw, err := json.Marshal(&domain.LimitError{
		Resource: domain.ResourceTables, Limit: 5, Current: 5, PlanCode: "free",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"resource", "limit", "current", "plan_code"} {
		if _, ok := got[key]; !ok {
			t.Errorf("serialised limit error is missing %q: %s", key, raw)
		}
	}
}

// ------------------------------------------------------------ state machines

func TestSubscriptionTransitions(t *testing.T) {
	allowed := []struct{ from, to domain.SubscriptionStatus }{
		{domain.SubTrialing, domain.SubActive},
		{domain.SubActive, domain.SubPastDue},
		{domain.SubPastDue, domain.SubActive},
		{domain.SubPastDue, domain.SubSuspended},
		{domain.SubSuspended, domain.SubActive},
		{domain.SubActive, domain.SubCancelled},
	}
	for _, tc := range allowed {
		if !tc.from.CanTransition(tc.to) {
			t.Errorf("%s -> %s should be allowed", tc.from, tc.to)
		}
	}

	refused := []struct{ from, to domain.SubscriptionStatus }{
		// Suspension is what the grace period leads to, never a jump from
		// active — otherwise a single failed charge locks an account out.
		{domain.SubActive, domain.SubSuspended},
		{domain.SubCancelled, domain.SubActive},
		{domain.SubTrialing, domain.SubSuspended},
	}
	for _, tc := range refused {
		if tc.from.CanTransition(tc.to) {
			t.Errorf("%s -> %s should be refused", tc.from, tc.to)
		}
	}
}

func TestOnlyLiveStatusesAreWritable(t *testing.T) {
	writable := map[domain.SubscriptionStatus]bool{
		domain.SubTrialing:  true,
		domain.SubActive:    true,
		domain.SubPastDue:   true,
		domain.SubSuspended: false,
		domain.SubCancelled: false,
	}
	for status, want := range writable {
		if got := status.Writable(); got != want {
			t.Errorf("%s writable = %v, want %v", status, got, want)
		}
	}
}

func TestInvoiceTransitions(t *testing.T) {
	if !domain.InvoiceOpen.CanTransition(domain.InvoicePaid) {
		t.Error("an open invoice should be payable")
	}
	if !domain.InvoicePaid.CanTransition(domain.InvoiceRefunded) {
		t.Error("a paid invoice should be refundable")
	}
	// A paid invoice must never go back to open: re-opening a settled bill
	// invites a second payment for a period already granted.
	if domain.InvoicePaid.CanTransition(domain.InvoiceOpen) {
		t.Error("a paid invoice must not return to open")
	}
	if domain.InvoiceVoid.CanTransition(domain.InvoicePaid) {
		t.Error("a voided invoice must not become paid")
	}
	if !domain.InvoiceOpen.Payable() || domain.InvoicePaid.Payable() {
		t.Error("only an open invoice is payable")
	}
}

func TestSubscriptionWithNoEndNeverExpires(t *testing.T) {
	free := domain.Subscription{CurrentPeriodEnd: nil}
	if free.Expired(time.Now().AddDate(10, 0, 0)) {
		t.Error("a subscription with no period end expired")
	}

	end := time.Now().Add(-time.Hour)
	lapsed := domain.Subscription{CurrentPeriodEnd: &end}
	if !lapsed.Expired(time.Now()) {
		t.Error("a period that ended an hour ago should read as expired")
	}
}

func TestIntervalMonths(t *testing.T) {
	if got := domain.IntervalMonth.Months(); got != 1 {
		t.Errorf("month = %d, want 1", got)
	}
	if got := domain.IntervalYear.Months(); got != 12 {
		t.Errorf("year = %d, want 12", got)
	}
}

func TestInvoiceNumberIsPaddedAndYearScoped(t *testing.T) {
	if got := domain.FormatInvoiceNumber(2026, 7); got != "INV-2026-0007" {
		t.Errorf("number = %q, want INV-2026-0007", got)
	}
}

// An abandoned checkout must eventually stop holding its invoice hostage.
func TestTransactionTimesOut(t *testing.T) {
	now := time.Now()
	fresh := domain.PaymentTransaction{State: domain.TxnCreated, CreatedMs: now.Add(-time.Hour).UnixMilli()}
	if fresh.Timeout(now) {
		t.Error("an hour-old transaction should not have timed out")
	}

	stale := domain.PaymentTransaction{
		State: domain.TxnCreated, CreatedMs: now.Add(-domain.MaxTxnAge - time.Minute).UnixMilli(),
	}
	if !stale.Timeout(now) {
		t.Error("a transaction older than MaxTxnAge should have timed out")
	}

	// A settled transaction is never "timed out": the money moved, and expiring
	// it would reverse a payment that succeeded.
	performed := domain.PaymentTransaction{
		State: domain.TxnPerformed, CreatedMs: now.Add(-30 * 24 * time.Hour).UnixMilli(),
	}
	if performed.Timeout(now) {
		t.Error("a performed transaction must never time out")
	}
}

// The message is the whole point of LimitError: a refusal a customer cannot act
// on is just a failure.
func TestLimitMessageReadsAsEnglish(t *testing.T) {
	tests := []struct {
		name string
		err  domain.LimitError
		want string
	}{
		{
			// The most common refusal in the product, and the one a plural
			// would make read like a bug.
			name: "a limit of one",
			err:  domain.LimitError{Resource: domain.ResourceRestaurants, Limit: 1, Current: 1, PlanCode: "free"},
			want: "Your free plan allows one restaurant. Upgrade to add more.",
		},
		{
			name: "a limit of several",
			err:  domain.LimitError{Resource: domain.ResourceTables, Limit: 5, Current: 5, PlanCode: "free"},
			want: "Your free plan allows 5 tables and you are using 5. Upgrade to add more.",
		},
		{
			name: "a limit of none",
			err:  domain.LimitError{Resource: domain.ResourceTables, Limit: 0, Current: 0, PlanCode: "free"},
			want: "Your free plan does not include tables. Upgrade to add them.",
		},
		{
			// menu_items must not surface with its underscore.
			name: "a multi-word resource",
			err:  domain.LimitError{Resource: domain.ResourceMenuItems, Limit: 30, Current: 30, PlanCode: "free"},
			want: "Your free plan allows 30 menu items and you are using 30. Upgrade to add more.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Message(); got != tc.want {
				t.Errorf("message = %q,\n     want %q", got, tc.want)
			}
		})
	}
}
