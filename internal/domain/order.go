package domain

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/i18n"
)

// OrderStatus is where an order is in its life.
type OrderStatus string

const (
	OrderPending   OrderStatus = "pending"   // placed, nobody has looked at it yet
	OrderAccepted  OrderStatus = "accepted"  // staff have seen it and taken it on
	OrderPreparing OrderStatus = "preparing" // the kitchen is cooking
	OrderReady     OrderStatus = "ready"     // waiting on the pass
	OrderServed    OrderStatus = "served"    // on the guest's table
	OrderClosed    OrderStatus = "closed"    // paid and done
	OrderCancelled OrderStatus = "cancelled"
)

// orderTransitions is the whole status machine, as data.
//
// Written out rather than checked with a pile of ifs because this is exactly
// where sloppy code produces orders that go backwards: a tablet that fires a
// stale request, two waiters tapping at once, a retry after a timeout. An
// explicit table also makes the one asymmetry visible — an order can be
// cancelled right up to the moment it reaches the table, and not after, because
// once the food is in front of the guest, cancelling it is a refund and not a
// status change.
var orderTransitions = map[OrderStatus][]OrderStatus{
	OrderPending:   {OrderAccepted, OrderCancelled},
	OrderAccepted:  {OrderPreparing, OrderReady, OrderCancelled},
	OrderPreparing: {OrderReady, OrderCancelled},
	OrderReady:     {OrderServed, OrderCancelled},
	OrderServed:    {OrderClosed},
	OrderClosed:    nil,
	OrderCancelled: nil,
}

func (s OrderStatus) Valid() bool {
	_, ok := orderTransitions[s]
	return ok
}

func (s OrderStatus) String() string { return string(s) }

// IsFinal reports whether an order will never change again.
func (s OrderStatus) IsFinal() bool { return len(orderTransitions[s]) == 0 }

// IsOpen reports whether an order still needs someone's attention, which is
// what the kitchen screen lists.
func (s OrderStatus) IsOpen() bool { return s.Valid() && !s.IsFinal() }

// CanTransition reports whether from → to is a legal move.
func (s OrderStatus) CanTransition(to OrderStatus) bool {
	return slices.Contains(orderTransitions[s], to)
}

// NextStatuses lists the legal moves, so a tablet can render exactly the
// buttons that will work instead of discovering a 409 on tap.
func (s OrderStatus) NextStatuses() []OrderStatus {
	return slices.Clone(orderTransitions[s])
}

// roleOrderStatuses is what each role is allowed to set, on top of the
// transition being legal in the first place. Both checks apply: the kitchen may
// not close an order even from 'served', and a waiter may not mark a dish
// 'preparing' when they are not the one cooking it.
//
// There is no hierarchy here either — owner is listed explicitly, the same way
// every route lists its roles.
var roleOrderStatuses = map[Role][]OrderStatus{
	RoleOwner:   {OrderAccepted, OrderPreparing, OrderReady, OrderServed, OrderClosed, OrderCancelled},
	RoleManager: {OrderAccepted, OrderPreparing, OrderReady, OrderServed, OrderClosed, OrderCancelled},
	RoleWaiter:  {OrderAccepted, OrderServed, OrderClosed, OrderCancelled},
	RoleKitchen: {OrderAccepted, OrderPreparing, OrderReady},
}

// RoleCanSetOrderStatus reports whether the role is permitted to move an order
// into that status at all.
func RoleCanSetOrderStatus(role Role, to OrderStatus) bool {
	return slices.Contains(roleOrderStatuses[role], to)
}

// Order is one guest's order, as it is stored and as staff see it.
type Order struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID

	// Nil once the table it came from has been deleted. TableLabel is the
	// snapshot and is always populated, so a ticket never loses its table.
	TableID    *uuid.UUID
	TableLabel string

	CodeSeq     int32
	BusinessDay time.Time
	Status      OrderStatus

	GuestSessionID string
	Locale         string
	Note           *string

	SubtotalMinor   int64
	ServiceFeeMinor int64
	TotalMinor      int64
	Currency        string

	Lines []OrderLine

	PlacedAt     time.Time
	AcceptedAt   *time.Time
	ReadyAt      *time.Time
	ClosedAt     *time.Time
	CancelReason *string
	UpdatedAt    time.Time
}

// Code is the number staff call across the room. It restarts daily per
// restaurant, which is what "order 42" already means to everyone in a dining
// room. Derived rather than stored, so the printed form and the sequence can
// never disagree.
func (o Order) Code() string { return FormatOrderCode(o.CodeSeq) }

func FormatOrderCode(seq int32) string { return fmt.Sprintf("%03d", seq) }

// OrderLine is one dish on the order, priced and named as it was at the moment
// it was placed. Nothing here is ever re-derived from the live menu.
type OrderLine struct {
	ID        uuid.UUID
	ItemID    *uuid.UUID
	VariantID *uuid.UUID

	Name        i18n.Text
	VariantName i18n.Text
	Modifiers   []OrderLineModifier

	Qty            int32
	UnitPriceMinor int64
	LineTotalMinor int64
	Note           *string
	SortOrder      int32
}

type OrderLineModifier struct {
	ID              uuid.UUID `json:"id"`
	Name            i18n.Text `json:"name"`
	PriceDeltaMinor int64     `json:"price_delta_minor"`
}

// ------------------------------------------------------------------ the cart

// Cart is what a guest sends. Note what it does not contain: prices. The client
// names things and says how many; every amount is computed here from the menu
// as it stands right now. A total that arrives in a request body is how an
// order for zero som gets placed.
type Cart struct {
	Lines []CartLine
	Note  string
}

type CartLine struct {
	ItemID      uuid.UUID
	VariantID   *uuid.UUID
	ModifierIDs []uuid.UUID
	Qty         int32
	Note        string
}

const (
	// MaxCartLines bounds one order. A table of ten ordering separately is
	// several orders, not one with two hundred lines.
	MaxCartLines = 50
	// MaxLineQty — ninety-nine of one dish is already implausible; the column
	// carries the same check so neither layer can be bypassed.
	MaxLineQty = 99
	// MaxOrderNoteLen and MaxLineNoteLen match the column constraints.
	MaxOrderNoteLen = 500
	MaxLineNoteLen  = 300
	// MaxOrderTotal is a sanity ceiling, not a business rule. Its real job is
	// arithmetic: price_minor is a bigint, so an owner who types a price near
	// int64 max would overflow unit_price × qty into a negative total and trip
	// a CHECK constraint as a 500. Rejecting the line as invalid says something
	// true instead.
	MaxOrderTotal = int64(1e15)
)

// ------------------------------------------------------------------- catalog

// Catalog is everything the pricer is allowed to see: the menu, resolved into
// lookups, with availability already decided.
//
// It exists so pricing is a pure function of (menu, cart). The service loads
// this from the database and hands it over; the rules below are then testable
// without a database, which matters because they are the rules that decide what
// a guest is charged.
type Catalog struct {
	Currency string
	Items    map[uuid.UUID]*CatalogItem
}

type CatalogItem struct {
	ID          uuid.UUID
	Name        i18n.Text
	PriceMinor  int64
	IsAvailable bool
	// Servable is false when the dish's category or menu is switched off, or
	// the menu is outside its serving window at the moment of ordering. The
	// public payload is deliberately time-independent and lets the renderer
	// decide what to show; an order is the opposite case — the server has to
	// decide, because the client's idea of the current time is not evidence.
	Servable bool

	Variants map[uuid.UUID]CatalogVariant
	Groups   map[uuid.UUID]*CatalogGroup
	// GroupOfModifier resolves a modifier straight to its group, so a selection
	// can be validated without scanning every group of the item.
	GroupOfModifier map[uuid.UUID]uuid.UUID
}

type CatalogVariant struct {
	ID              uuid.UUID
	Name            i18n.Text
	PriceDeltaMinor int64
	IsAvailable     bool
}

type CatalogGroup struct {
	ID        uuid.UUID
	Name      i18n.Text
	MinSelect int32
	MaxSelect int32
	Modifiers map[uuid.UUID]CatalogModifier
}

type CatalogModifier struct {
	ID              uuid.UUID
	Name            i18n.Text
	PriceDeltaMinor int64
	IsAvailable     bool
}

// PricedCart is the result of validating and pricing a cart.
type PricedCart struct {
	Lines           []OrderLine
	SubtotalMinor   int64
	ServiceFeeMinor int64
	TotalMinor      int64
	Currency        string
}

// PriceCart validates a cart against the menu and computes what it costs.
//
// Every problem is collected rather than returning on the first, so a guest
// whose cart went stale while they browsed is told everything that changed in
// one go instead of fixing it one dish per round trip. Field paths are indexed
// ("lines[2].modifier_ids") so a client can highlight the offending row.
func PriceCart(cat Catalog, cart Cart) (PricedCart, error) {
	var v Validator

	note := strings.TrimSpace(cart.Note)
	v.Check(utf8.RuneCountInString(note) <= MaxOrderNoteLen,
		"note", "must be at most %d characters", MaxOrderNoteLen)

	v.Check(len(cart.Lines) > 0, "lines", "must contain at least one item")
	v.Check(len(cart.Lines) <= MaxCartLines, "lines", "must contain at most %d items", MaxCartLines)
	if !v.OK() {
		return PricedCart{}, v.Err()
	}

	lines := make([]OrderLine, 0, len(cart.Lines))
	var subtotal int64

	for i, in := range cart.Lines {
		field := func(name string) string { return fmt.Sprintf("lines[%d].%s", i, name) }

		item, ok := cat.Items[in.ItemID]
		if !ok {
			// Deliberately the same message for "never existed" and "belongs to
			// another restaurant": the catalog only ever holds this
			// restaurant's dishes, so a guest cannot use the error to learn
			// whether an id is real somewhere else on the platform.
			v.Add(field("item_id"), "is not on this menu")
			continue
		}
		if !item.Servable {
			v.Add(field("item_id"), "is not being served right now")
			continue
		}
		if !item.IsAvailable {
			v.Add(field("item_id"), "is sold out")
			continue
		}

		if in.Qty < 1 || in.Qty > MaxLineQty {
			v.Add(field("qty"), "must be between 1 and %d", MaxLineQty)
			continue
		}

		lineNote := strings.TrimSpace(in.Note)
		if utf8.RuneCountInString(lineNote) > MaxLineNoteLen {
			v.Add(field("note"), "must be at most %d characters", MaxLineNoteLen)
			continue
		}

		unit := item.PriceMinor
		line := OrderLine{
			ItemID:    &item.ID,
			Name:      item.Name,
			Qty:       in.Qty,
			SortOrder: int32(i),
		}
		if lineNote != "" {
			line.Note = &lineNote
		}

		if in.VariantID != nil {
			variant, ok := item.Variants[*in.VariantID]
			switch {
			case !ok:
				v.Add(field("variant_id"), "is not an option for this dish")
				continue
			case !variant.IsAvailable:
				v.Add(field("variant_id"), "is not available")
				continue
			}
			unit += variant.PriceDeltaMinor
			line.VariantID = &variant.ID
			line.VariantName = variant.Name
		}

		modifiers, delta, ok := priceModifiers(&v, field, item, in.ModifierIDs)
		if !ok {
			continue
		}
		unit += delta
		line.Modifiers = modifiers

		// A negative unit price would mean the modifiers discount the dish
		// below free. Prices and deltas are owner-controlled, so this is a
		// misconfigured menu rather than a hostile guest — but it must not
		// become a negative total.
		if unit < 0 {
			v.Add(field("item_id"), "is priced below zero once the selected options are applied")
			continue
		}
		if unit > MaxOrderTotal/int64(in.Qty) {
			v.Add(field("qty"), "makes the order total implausibly large")
			continue
		}

		line.UnitPriceMinor = unit
		line.LineTotalMinor = unit * int64(in.Qty)

		if subtotal > MaxOrderTotal-line.LineTotalMinor {
			v.Add("lines", "total is implausibly large")
			break
		}
		subtotal += line.LineTotalMinor
		lines = append(lines, line)
	}

	if err := v.Err(); err != nil {
		return PricedCart{}, err
	}

	// There is no fee policy yet. The field is carried through rather than
	// omitted so that adding one later changes a calculation, not a schema.
	const serviceFee = int64(0)

	return PricedCart{
		Lines:           lines,
		SubtotalMinor:   subtotal,
		ServiceFeeMinor: serviceFee,
		TotalMinor:      subtotal + serviceFee,
		Currency:        cat.Currency,
	}, nil
}

// priceModifiers validates one line's extras and returns their combined delta.
//
// The group rules are the fiddly part and the reason this is separate: a
// selection is only valid as a *set*, so "at least one sauce" and "at most two
// toppings" cannot be checked one modifier at a time.
func priceModifiers(v *Validator, field func(string) string, item *CatalogItem, ids []uuid.UUID) ([]OrderLineModifier, int64, bool) {
	selected := make(map[uuid.UUID][]CatalogModifier, len(item.Groups))
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]OrderLineModifier, 0, len(ids))
	var delta int64
	ok := true

	for _, id := range ids {
		if _, dup := seen[id]; dup {
			// Two of the same extra is a client bug, not "double cheese" —
			// quantity per modifier is not a thing the menu can express, so
			// silently charging twice would be inventing a price.
			v.Add(field("modifier_ids"), "lists the same option twice")
			ok = false
			continue
		}
		seen[id] = struct{}{}

		groupID, found := item.GroupOfModifier[id]
		if !found {
			v.Add(field("modifier_ids"), "contains an option that does not belong to this dish")
			ok = false
			continue
		}
		group := item.Groups[groupID]
		modifier := group.Modifiers[id]
		if !modifier.IsAvailable {
			v.Add(field("modifier_ids"), "contains an option that is unavailable")
			ok = false
			continue
		}

		selected[groupID] = append(selected[groupID], modifier)
		delta += modifier.PriceDeltaMinor
		out = append(out, OrderLineModifier{
			ID:              modifier.ID,
			Name:            modifier.Name,
			PriceDeltaMinor: modifier.PriceDeltaMinor,
		})
	}

	// Every group of the dish is checked, not just the ones the guest touched:
	// a required group left out entirely is the case that matters, and it has
	// no entry in `selected` to iterate over.
	for groupID, group := range item.Groups {
		n := int32(len(selected[groupID]))
		name := group.Name.Get("", "")
		switch {
		case n < group.MinSelect:
			v.Add(field("modifier_ids"), "requires at least %d option(s) from %q", group.MinSelect, name)
			ok = false
		case n > group.MaxSelect:
			v.Add(field("modifier_ids"), "allows at most %d option(s) from %q", group.MaxSelect, name)
			ok = false
		}
	}

	return out, delta, ok
}

// -------------------------------------------------------------- guest session

// GuestSessionBytes is the entropy behind the cookie that lets an anonymous
// guest read back the order they just placed. It is the only credential in that
// flow, so it is sized like one.
const GuestSessionBytes = 32

// NewGuestSession returns an opaque session id for the guest cookie.
func NewGuestSession() (string, error) {
	buf := make([]byte, GuestSessionBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidGuestSession rejects a cookie value that could not have come from
// NewGuestSession, so a hand-crafted one never reaches a query.
func ValidGuestSession(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// -------------------------------------------------------------- waiter calls

type WaiterCallKind string

const (
	CallWaiter WaiterCallKind = "waiter" // come to the table
	CallBill   WaiterCallKind = "bill"   // we would like to pay
	CallHelp   WaiterCallKind = "help"   // something is wrong
)

var allCallKinds = []WaiterCallKind{CallWaiter, CallBill, CallHelp}

func (k WaiterCallKind) Valid() bool { return slices.Contains(allCallKinds, k) }

func (k WaiterCallKind) String() string { return string(k) }

func AllCallKinds() []WaiterCallKind { return slices.Clone(allCallKinds) }

type WaiterCallStatus string

const (
	CallPending      WaiterCallStatus = "pending"
	CallAcknowledged WaiterCallStatus = "acknowledged"
	CallResolved     WaiterCallStatus = "resolved"
)

var callTransitions = map[WaiterCallStatus][]WaiterCallStatus{
	CallPending:      {CallAcknowledged, CallResolved},
	CallAcknowledged: {CallResolved},
	CallResolved:     nil,
}

func (s WaiterCallStatus) Valid() bool {
	_, ok := callTransitions[s]
	return ok
}

func (s WaiterCallStatus) String() string { return string(s) }

func (s WaiterCallStatus) CanTransition(to WaiterCallStatus) bool {
	return slices.Contains(callTransitions[s], to)
}

type WaiterCall struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	TableID      *uuid.UUID
	TableLabel   string
	Kind         WaiterCallKind
	Status       WaiterCallStatus
	Note         *string

	CreatedAt      time.Time
	AcknowledgedAt *time.Time
	ResolvedAt     *time.Time
}

// ValidateCallNote is the whole of a waiter call's input validation — the kind
// is checked by its own type and the table comes from the QR token.
func ValidateCallNote(note string) (string, error) {
	var v Validator
	note = strings.TrimSpace(note)
	v.Check(utf8.RuneCountInString(note) <= MaxLineNoteLen,
		"note", "must be at most %d characters", MaxLineNoteLen)
	return note, v.Err()
}
