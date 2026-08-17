package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/i18n"
)

// The status machine and the pricer are the two things in this package that
// decide what a guest is charged and whether an order can go backwards, so they
// are pinned here rather than exercised only through the database.

func TestOrderTransitions(t *testing.T) {
	t.Parallel()

	// Legal moves, and the ones that look plausible but must not be.
	cases := []struct {
		from, to OrderStatus
		want     bool
		why      string
	}{
		{OrderPending, OrderAccepted, true, "the normal first move"},
		{OrderAccepted, OrderPreparing, true, "into the kitchen"},
		{OrderAccepted, OrderReady, true, "a drink needs no preparing step"},
		{OrderPreparing, OrderReady, true, "off the stove"},
		{OrderReady, OrderServed, true, "onto the table"},
		{OrderServed, OrderClosed, true, "paid"},

		{OrderPending, OrderCancelled, true, "cancelled before anyone looked"},
		{OrderPreparing, OrderCancelled, true, "cancelled mid-cook"},
		{OrderReady, OrderCancelled, true, "cancelled on the pass"},

		{OrderServed, OrderCancelled, false, "food already on the table is a refund, not a cancellation"},
		{OrderClosed, OrderCancelled, false, "a closed order is final"},
		{OrderCancelled, OrderPending, false, "cancelled is final"},
		{OrderReady, OrderPreparing, false, "an order must never go backwards"},
		{OrderServed, OrderReady, false, "nor this way"},
		{OrderPending, OrderServed, false, "cannot skip to served"},
		{OrderPending, OrderClosed, false, "cannot skip to closed"},
	}

	for _, c := range cases {
		if got := c.from.CanTransition(c.to); got != c.want {
			t.Errorf("%s -> %s = %v, want %v (%s)", c.from, c.to, got, c.want, c.why)
		}
	}
}

func TestOrderStatusFinality(t *testing.T) {
	t.Parallel()

	for _, s := range []OrderStatus{OrderClosed, OrderCancelled} {
		if !s.IsFinal() {
			t.Errorf("%s should be final", s)
		}
		if s.IsOpen() {
			t.Errorf("%s should not appear on the open board", s)
		}
	}
	for _, s := range []OrderStatus{OrderPending, OrderAccepted, OrderPreparing, OrderReady, OrderServed} {
		if !s.IsOpen() {
			t.Errorf("%s should appear on the open board", s)
		}
	}
	if OrderStatus("delivered").Valid() {
		t.Error("an unknown status must not validate")
	}
}

// Every status a role may set has to be reachable by that role from somewhere,
// otherwise the permission is decoration.
func TestRoleOrderStatusesAreReachable(t *testing.T) {
	t.Parallel()

	for role, allowed := range roleOrderStatuses {
		for _, to := range allowed {
			reachable := false
			for from := range orderTransitions {
				if from.CanTransition(to) {
					reachable = true
					break
				}
			}
			if !reachable {
				t.Errorf("role %s may set %s, but no transition leads there", role, to)
			}
		}
	}
}

func TestRoleCanSetOrderStatus(t *testing.T) {
	t.Parallel()

	// The kitchen cooks; it does not take payment or write off a bill.
	if RoleCanSetOrderStatus(RoleKitchen, OrderClosed) {
		t.Error("kitchen must not be able to close an order")
	}
	if RoleCanSetOrderStatus(RoleKitchen, OrderCancelled) {
		t.Error("kitchen must not be able to cancel an order")
	}
	if !RoleCanSetOrderStatus(RoleKitchen, OrderReady) {
		t.Error("kitchen must be able to mark food ready")
	}
	// A waiter is not the one at the stove.
	if RoleCanSetOrderStatus(RoleWaiter, OrderPreparing) {
		t.Error("waiter must not mark an order as preparing")
	}
	if !RoleCanSetOrderStatus(RoleWaiter, OrderServed) {
		t.Error("waiter must be able to mark an order served")
	}
	if !RoleCanSetOrderStatus(RoleOwner, OrderClosed) {
		t.Error("owner must be able to close an order")
	}
}

func TestFormatOrderCode(t *testing.T) {
	t.Parallel()

	cases := map[int32]string{1: "001", 42: "042", 999: "999", 1000: "1000"}
	for seq, want := range cases {
		if got := FormatOrderCode(seq); got != want {
			t.Errorf("FormatOrderCode(%d) = %q, want %q", seq, got, want)
		}
	}
}

// ------------------------------------------------------------------ pricing

var (
	plovID    = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	largeID   = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	beefID    = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	saladID   = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	sauceID   = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	ketchupID = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	unknownID = uuid.MustParse("99999999-9999-4999-8999-999999999999")

	meatGroupID  = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	extraGroupID = uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
)

// testCatalog is one plov at 45 000 so'm with a "large" variant (+15 000), a
// required meat choice (beef +5 000) and an optional sauce (+2 000), plus a
// plain salad. UZS is a zero-decimal currency here, so these are whole so'm.
func testCatalog() Catalog {
	return Catalog{
		Currency: "UZS",
		Items: map[uuid.UUID]*CatalogItem{
			plovID: {
				ID:          plovID,
				Name:        i18n.Text{"uz": "Osh", "ru": "Плов"},
				PriceMinor:  45000,
				IsAvailable: true,
				Servable:    true,
				Variants: map[uuid.UUID]CatalogVariant{
					largeID: {ID: largeID, Name: i18n.Text{"uz": "Katta"}, PriceDeltaMinor: 15000, IsAvailable: true},
				},
				Groups: map[uuid.UUID]*CatalogGroup{
					meatGroupID: {
						ID: meatGroupID, Name: i18n.Text{"uz": "Go'sht"},
						MinSelect: 1, MaxSelect: 1,
						Modifiers: map[uuid.UUID]CatalogModifier{
							beefID: {ID: beefID, Name: i18n.Text{"uz": "Mol"}, PriceDeltaMinor: 5000, IsAvailable: true},
						},
					},
					extraGroupID: {
						ID: extraGroupID, Name: i18n.Text{"uz": "Qo'shimcha"},
						MinSelect: 0, MaxSelect: 1,
						Modifiers: map[uuid.UUID]CatalogModifier{
							sauceID: {ID: sauceID, Name: i18n.Text{"uz": "Sous"}, PriceDeltaMinor: 2000, IsAvailable: true},
						},
					},
				},
				GroupOfModifier: map[uuid.UUID]uuid.UUID{
					beefID:  meatGroupID,
					sauceID: extraGroupID,
				},
			},
			saladID: {
				ID:              saladID,
				Name:            i18n.Text{"uz": "Achichuk"},
				PriceMinor:      12000,
				IsAvailable:     true,
				Servable:        true,
				Variants:        map[uuid.UUID]CatalogVariant{},
				Groups:          map[uuid.UUID]*CatalogGroup{},
				GroupOfModifier: map[uuid.UUID]uuid.UUID{},
			},
		},
	}
}

func TestPriceCartComputesTotals(t *testing.T) {
	t.Parallel()

	cart := Cart{Lines: []CartLine{
		{ItemID: plovID, VariantID: &largeID, ModifierIDs: []uuid.UUID{beefID, sauceID}, Qty: 2},
		{ItemID: saladID, Qty: 1},
	}}

	priced, err := PriceCart(testCatalog(), cart)
	if err != nil {
		t.Fatalf("PriceCart: %v", err)
	}

	// 45 000 + 15 000 (large) + 5 000 (beef) + 2 000 (sauce) = 67 000 each.
	const plovUnit = 67000
	if got := priced.Lines[0].UnitPriceMinor; got != plovUnit {
		t.Errorf("plov unit price = %d, want %d", got, plovUnit)
	}
	if got := priced.Lines[0].LineTotalMinor; got != plovUnit*2 {
		t.Errorf("plov line total = %d, want %d", got, plovUnit*2)
	}
	if got := priced.SubtotalMinor; got != plovUnit*2+12000 {
		t.Errorf("subtotal = %d, want %d", got, plovUnit*2+12000)
	}
	if priced.TotalMinor != priced.SubtotalMinor+priced.ServiceFeeMinor {
		t.Errorf("total %d does not equal subtotal %d + fee %d",
			priced.TotalMinor, priced.SubtotalMinor, priced.ServiceFeeMinor)
	}
	if priced.Currency != "UZS" {
		t.Errorf("currency = %q, want UZS", priced.Currency)
	}

	// The snapshot is the whole point: the ticket must carry the name and the
	// options as they were, not a pointer to a menu that will change.
	if priced.Lines[0].Name["ru"] != "Плов" {
		t.Errorf("name snapshot lost the Russian translation: %v", priced.Lines[0].Name)
	}
	if len(priced.Lines[0].Modifiers) != 2 {
		t.Errorf("expected 2 modifiers snapshotted, got %d", len(priced.Lines[0].Modifiers))
	}
	// Line order has to survive, or the kitchen ticket reshuffles itself.
	if priced.Lines[0].SortOrder != 0 || priced.Lines[1].SortOrder != 1 {
		t.Errorf("sort order not preserved: %d, %d", priced.Lines[0].SortOrder, priced.Lines[1].SortOrder)
	}
}

// Each of these is a way a cart can be wrong, and each must be reported rather
// than silently priced.
func TestPriceCartRejects(t *testing.T) {
	t.Parallel()

	soldOut := func(c Catalog) Catalog { c.Items[plovID].IsAvailable = false; return c }
	offMenu := func(c Catalog) Catalog { c.Items[plovID].Servable = false; return c }
	// Adds a second sauce to a group that still permits only one, so a cart can
	// name two distinct valid options and still be over the limit.
	twoSauces := func(c Catalog) Catalog {
		c.Items[plovID].Groups[extraGroupID].Modifiers[ketchupID] = CatalogModifier{
			ID: ketchupID, Name: i18n.Text{"uz": "Ketchup"}, PriceDeltaMinor: 1000, IsAvailable: true,
		}
		c.Items[plovID].GroupOfModifier[ketchupID] = extraGroupID
		return c
	}
	noStock := func(c Catalog) Catalog {
		c.Items[plovID].Groups[meatGroupID].Modifiers[beefID] = CatalogModifier{
			ID: beefID, Name: i18n.Text{"uz": "Mol"}, PriceDeltaMinor: 5000, IsAvailable: false,
		}
		return c
	}

	cases := []struct {
		name    string
		mutate  func(Catalog) Catalog
		cart    Cart
		field   string
		message string
	}{
		{
			name:    "empty cart",
			cart:    Cart{},
			field:   "lines",
			message: "at least one item",
		},
		{
			name:    "unknown item",
			cart:    Cart{Lines: []CartLine{{ItemID: unknownID, Qty: 1}}},
			field:   "lines[0].item_id",
			message: "not on this menu",
		},
		{
			name:    "sold out",
			mutate:  soldOut,
			cart:    Cart{Lines: []CartLine{{ItemID: plovID, ModifierIDs: []uuid.UUID{beefID}, Qty: 1}}},
			field:   "lines[0].item_id",
			message: "sold out",
		},
		{
			name:    "menu not being served now",
			mutate:  offMenu,
			cart:    Cart{Lines: []CartLine{{ItemID: plovID, ModifierIDs: []uuid.UUID{beefID}, Qty: 1}}},
			field:   "lines[0].item_id",
			message: "not being served right now",
		},
		{
			name:    "zero quantity",
			cart:    Cart{Lines: []CartLine{{ItemID: saladID, Qty: 0}}},
			field:   "lines[0].qty",
			message: "between 1 and",
		},
		{
			name:    "quantity beyond the cap",
			cart:    Cart{Lines: []CartLine{{ItemID: saladID, Qty: MaxLineQty + 1}}},
			field:   "lines[0].qty",
			message: "between 1 and",
		},
		{
			name:    "variant from another dish",
			cart:    Cart{Lines: []CartLine{{ItemID: saladID, VariantID: &largeID, Qty: 1}}},
			field:   "lines[0].variant_id",
			message: "not an option for this dish",
		},
		{
			name:    "required group not chosen",
			cart:    Cart{Lines: []CartLine{{ItemID: plovID, Qty: 1}}},
			field:   "lines[0].modifier_ids",
			message: "at least 1 option",
		},
		{
			name:    "same option listed twice",
			cart:    Cart{Lines: []CartLine{{ItemID: plovID, ModifierIDs: []uuid.UUID{beefID, sauceID, sauceID}, Qty: 1}}},
			field:   "lines[0].modifier_ids",
			message: "same option twice",
		},
		{
			// Distinct options, both legitimate, but one more than the group
			// allows — the case the duplicate check above does not cover.
			name:    "too many from one group",
			mutate:  twoSauces,
			cart:    Cart{Lines: []CartLine{{ItemID: plovID, ModifierIDs: []uuid.UUID{beefID, sauceID, ketchupID}, Qty: 1}}},
			field:   "lines[0].modifier_ids",
			message: "at most 1 option",
		},
		{
			name:    "modifier from another dish",
			cart:    Cart{Lines: []CartLine{{ItemID: saladID, ModifierIDs: []uuid.UUID{sauceID}, Qty: 1}}},
			field:   "lines[0].modifier_ids",
			message: "does not belong to this dish",
		},
		{
			name:    "unavailable modifier",
			mutate:  noStock,
			cart:    Cart{Lines: []CartLine{{ItemID: plovID, ModifierIDs: []uuid.UUID{beefID}, Qty: 1}}},
			field:   "lines[0].modifier_ids",
			message: "unavailable",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			catalog := testCatalog()
			if c.mutate != nil {
				catalog = c.mutate(catalog)
			}

			_, err := PriceCart(catalog, c.cart)
			if err == nil {
				t.Fatal("expected the cart to be rejected, it was priced")
			}

			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("expected a ValidationError, got %T: %v", err, err)
			}

			for _, f := range validationErr.Fields {
				if f.Field == c.field && strings.Contains(f.Message, c.message) {
					return
				}
			}
			t.Errorf("no error on %q containing %q; got %v", c.field, c.message, validationErr.Fields)
		})
	}
}

// A cart naming several bad dishes should come back describing all of them, so
// a guest whose cart went stale fixes it in one round trip.
func TestPriceCartReportsEveryBadLine(t *testing.T) {
	t.Parallel()

	cart := Cart{Lines: []CartLine{
		{ItemID: unknownID, Qty: 1},
		{ItemID: saladID, Qty: 1},
		{ItemID: saladID, Qty: 0},
	}}

	_, err := PriceCart(testCatalog(), cart)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected a ValidationError, got %v", err)
	}
	if len(validationErr.Fields) != 2 {
		t.Fatalf("expected both bad lines reported, got %d: %v",
			len(validationErr.Fields), validationErr.Fields)
	}
	if validationErr.Fields[0].Field != "lines[0].item_id" ||
		validationErr.Fields[1].Field != "lines[2].qty" {
		t.Errorf("field paths must be indexed by the line the client sent: %v", validationErr.Fields)
	}
}

// price_minor is a bigint, so an owner who fat-fingers a price near int64 max
// would overflow unit × qty into a negative total and trip a CHECK constraint
// as a 500. The guard turns that into a message that says something true.
func TestPriceCartRefusesOverflow(t *testing.T) {
	t.Parallel()

	catalog := testCatalog()
	catalog.Items[saladID].PriceMinor = 1 << 62

	_, err := PriceCart(catalog, Cart{Lines: []CartLine{{ItemID: saladID, Qty: 2}}})
	if err == nil {
		t.Fatal("expected an implausible total to be refused")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected a ValidationError, got %T", err)
	}
}

// Modifier deltas may be negative (a discount for leaving something out), but
// they must never push a dish below free.
func TestPriceCartRefusesNegativeLine(t *testing.T) {
	t.Parallel()

	catalog := testCatalog()
	catalog.Items[plovID].PriceMinor = 1000
	catalog.Items[plovID].Groups[meatGroupID].Modifiers[beefID] = CatalogModifier{
		ID: beefID, Name: i18n.Text{"uz": "Mol"}, PriceDeltaMinor: -50000, IsAvailable: true,
	}

	_, err := PriceCart(catalog, Cart{Lines: []CartLine{
		{ItemID: plovID, ModifierIDs: []uuid.UUID{beefID}, Qty: 1},
	}})
	if err == nil {
		t.Fatal("expected a below-zero line to be refused")
	}
}

func TestPriceCartRefusesTooManyLines(t *testing.T) {
	t.Parallel()

	lines := make([]CartLine, MaxCartLines+1)
	for i := range lines {
		lines[i] = CartLine{ItemID: saladID, Qty: 1}
	}
	if _, err := PriceCart(testCatalog(), Cart{Lines: lines}); err == nil {
		t.Fatalf("expected more than %d lines to be refused", MaxCartLines)
	}
}

// ------------------------------------------------------------ guest sessions

func TestGuestSession(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 100)
	for range 100 {
		s, err := NewGuestSession()
		if err != nil {
			t.Fatalf("NewGuestSession: %v", err)
		}
		if !ValidGuestSession(s) {
			t.Fatalf("a freshly generated session did not validate: %q", s)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate session id generated: %q", s)
		}
		seen[s] = struct{}{}
	}

	// The column requires 16..128 characters and the value must decode, so a
	// hand-crafted cookie never reaches a query.
	for _, bad := range []string{"", "short", strings.Repeat("a", 200), "not base64!!"} {
		if ValidGuestSession(bad) {
			t.Errorf("%q should not validate as a guest session", bad)
		}
	}
}

// ------------------------------------------------------------- waiter calls

func TestWaiterCallTransitions(t *testing.T) {
	t.Parallel()

	if !CallPending.CanTransition(CallAcknowledged) {
		t.Error("a pending call should be acknowledgeable")
	}
	// A waiter who is already on their way should be able to close it out
	// directly, without tapping twice.
	if !CallPending.CanTransition(CallResolved) {
		t.Error("a pending call should be resolvable directly")
	}
	if CallAcknowledged.CanTransition(CallPending) {
		t.Error("a call must not go back to pending")
	}
	if CallResolved.CanTransition(CallAcknowledged) {
		t.Error("a resolved call is final")
	}
	if WaiterCallKind("shout").Valid() {
		t.Error("an unknown call kind must not validate")
	}
}
