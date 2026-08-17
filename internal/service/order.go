package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
	"github.com/ubilolxon/qrmenu/internal/realtime"
)

// Order is the ordering surface: guests place orders from a table, staff move
// them through the kitchen, and both sides see the result immediately.
type Order struct {
	store  *postgres.Store
	hub    *realtime.Hub
	logger *slog.Logger
}

func NewOrder(store *postgres.Store, hub *realtime.Hub, logger *slog.Logger) *Order {
	return &Order{store: store, hub: hub, logger: logger}
}

// Hub exposes the event registry to the SSE handler, which needs to subscribe
// but has no business publishing.
func (s *Order) Hub() *realtime.Hub { return s.hub }

// IdempotencyWindow is how long a key may be replayed for.
//
// A guest's phone retries within seconds; ten minutes is generous for a bad
// connection and short enough that a client reusing keys carelessly finds out
// rather than silently receiving a stale order. Past the window the key is
// refused outright instead of quietly creating a second order — the whole point
// is that a double-tap can never put two tickets in the kitchen.
const IdempotencyWindow = 10 * time.Minute

// codeAttempts bounds the retry when two orders race for the same daily number.
// The number is allocated as max+1 and made safe by a unique index, so a
// collision means simultaneous orders, not a bug — and a busy restaurant will
// not lose three in a row.
const codeAttempts = 5

// PlaceInput is a guest's order as it arrives, before anything is trusted.
type PlaceInput struct {
	Cart           domain.Cart
	Locale         string
	GuestSessionID string
	IdempotencyKey string
}

// Placed reports what happened, so the handler can answer 201 for a new order
// and 200 for a replayed one.
type Placed struct {
	Order    domain.Order
	Replayed bool
}

// Called is the same distinction for waiter calls: pressing the button while a
// call is already live returns the existing one, and 201 Created would be a
// lie about a row that was not created.
type Called struct {
	Call    domain.WaiterCall
	Created bool
}

// Place validates a cart against the live menu, prices it, and records it.
//
// The table comes from the QR token rather than the request body: a guest can
// only order for the table they are sitting at, and there is nothing to spoof
// because the token is the only table identifier they ever see.
func (s *Order) Place(ctx context.Context, token, guestSessionID string, in PlaceInput) (Placed, error) {
	if !domain.ValidGuestSession(guestSessionID) {
		return Placed{}, fmt.Errorf("place order: %w", domain.Errorf(domain.ErrForbidden,
			"A guest session is required to place an order."))
	}

	token = domain.NormalizeQRToken(token)
	if !domain.ValidQRTokenShape(token) {
		return Placed{}, domain.ErrNotFound
	}
	row, err := s.store.GetTableByToken(ctx, token)
	if err != nil {
		return Placed{}, wrap("get table", err)
	}
	table, restaurant := row.RestaurantTable, row.Restaurant

	if replayed, ok, err := s.replay(ctx, table.ID, in.IdempotencyKey); err != nil {
		return Placed{}, err
	} else if ok {
		return Placed{Order: replayed, Replayed: true}, nil
	}

	// The clock and the day number come from Postgres in one call, so the
	// serving-window check below and the order's business day cannot disagree.
	alloc, err := s.store.NextOrderCode(ctx, gen.NextOrderCodeParams{
		Timezone: restaurant.Timezone, RestaurantID: restaurant.ID,
	})
	if err != nil {
		return Placed{}, wrap("allocate order code", err)
	}

	catalog, err := s.catalog(ctx, restaurant, alloc.LocalNow, in.Cart)
	if err != nil {
		return Placed{}, err
	}

	priced, err := domain.PriceCart(catalog, in.Cart)
	if err != nil {
		return Placed{}, err
	}

	locale := domain.ResolveLocale(in.Locale, restaurant.Locales, restaurant.DefaultLocale)
	created, err := s.insert(ctx, table, restaurant, alloc, priced, locale, guestSessionID, in)
	if err != nil {
		return Placed{}, err
	}

	s.publish(ctx, restaurant.ID, realtime.EventOrderCreated, created.View())
	return Placed{Order: created}, nil
}

// replay looks for an order already placed under this idempotency key.
func (s *Order) replay(ctx context.Context, tableID uuid.UUID, key string) (domain.Order, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return domain.Order{}, false, nil
	}

	existing, err := s.store.GetOrderByIdempotencyKey(ctx, gen.GetOrderByIdempotencyKeyParams{
		TableID: &tableID, IdempotencyKey: &key,
	})
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return domain.Order{}, false, nil
		}
		return domain.Order{}, false, wrap("check idempotency key", err)
	}

	if time.Since(existing.PlacedAt) > IdempotencyWindow {
		return domain.Order{}, false, domain.Errorf(domain.ErrConflict,
			"That Idempotency-Key was already used for an earlier order. Use a new one.")
	}

	lines, err := s.store.ListOrderItems(ctx, existing.ID)
	if err != nil {
		return domain.Order{}, false, wrap("list order items", err)
	}
	return postgres.ToOrder(existing, lines), true, nil
}

// insert writes the order and its lines in one transaction, retrying when two
// orders race for the same daily number.
//
// Everything is one transaction because an order with no lines is not a
// smaller order, it is a corrupt one: the kitchen would get a ticket for
// nothing and the totals would describe food that was never requested.
func (s *Order) insert(
	ctx context.Context,
	table gen.RestaurantTable,
	restaurant gen.Restaurant,
	alloc gen.NextOrderCodeRow,
	priced domain.PricedCart,
	locale, guestSessionID string,
	in PlaceInput,
) (domain.Order, error) {
	params := gen.CreateOrderParams{
		RestaurantID:    restaurant.ID,
		TableID:         &table.ID,
		TableLabel:      table.Label,
		CodeSeq:         alloc.NextSeq,
		BusinessDay:     alloc.BusinessDay,
		GuestSessionID:  guestSessionID,
		Locale:          locale,
		SubtotalMinor:   priced.SubtotalMinor,
		ServiceFeeMinor: priced.ServiceFeeMinor,
		TotalMinor:      priced.TotalMinor,
		Currency:        priced.Currency,
	}
	if note := strings.TrimSpace(in.Cart.Note); note != "" {
		params.Note = &note
	}
	if key := strings.TrimSpace(in.IdempotencyKey); key != "" {
		params.IdempotencyKey = &key
	}

	for attempt := range codeAttempts {
		var order gen.Order
		var lines []gen.OrderItem

		err := s.store.InTx(ctx, func(q *gen.Queries) error {
			var err error
			if order, err = q.CreateOrder(ctx, params); err != nil {
				return err
			}
			lines = make([]gen.OrderItem, 0, len(priced.Lines))
			for _, line := range priced.Lines {
				row, err := insertLine(ctx, q, order.ID, restaurant.ID, line)
				if err != nil {
					return err
				}
				lines = append(lines, row)
			}
			return nil
		})
		if err == nil {
			return postgres.ToOrder(order, lines), nil
		}

		mapped := postgres.MapError(err)
		// A duplicate number is ours to fix; a duplicate idempotency key means
		// a concurrent retry of this very request won the race, so re-read it
		// rather than reporting a conflict for something that succeeded.
		if isOrderCodeCollision(mapped) && attempt < codeAttempts-1 {
			alloc, err = s.store.NextOrderCode(ctx, gen.NextOrderCodeParams{
				Timezone: restaurant.Timezone, RestaurantID: restaurant.ID,
			})
			if err != nil {
				return domain.Order{}, wrap("allocate order code", err)
			}
			params.CodeSeq, params.BusinessDay = alloc.NextSeq, alloc.BusinessDay
			continue
		}
		if isIdempotencyCollision(mapped) {
			if replayed, ok, rerr := s.replay(ctx, table.ID, in.IdempotencyKey); rerr == nil && ok {
				return replayed, nil
			}
		}
		return domain.Order{}, fmt.Errorf("create order: %w", mapped)
	}

	return domain.Order{}, fmt.Errorf("create order: could not allocate an order number in %d attempts", codeAttempts)
}

func insertLine(ctx context.Context, q *gen.Queries, orderID, restaurantID uuid.UUID, line domain.OrderLine) (gen.OrderItem, error) {
	name, err := line.Name.Raw()
	if err != nil {
		return gen.OrderItem{}, fmt.Errorf("encode item name: %w", err)
	}
	// json.Marshal of a nil slice is "null"; the column defaults to '[]' and
	// naming it explicitly in the INSERT sends whatever we pass, so an empty
	// list has to be spelled out.
	modifiers := line.Modifiers
	if modifiers == nil {
		modifiers = []domain.OrderLineModifier{}
	}
	rawModifiers, err := json.Marshal(modifiers)
	if err != nil {
		return gen.OrderItem{}, fmt.Errorf("encode modifiers: %w", err)
	}

	params := gen.CreateOrderItemParams{
		OrderID:        orderID,
		RestaurantID:   restaurantID,
		ItemID:         line.ItemID,
		VariantID:      line.VariantID,
		Qty:            line.Qty,
		UnitPriceMinor: line.UnitPriceMinor,
		LineTotalMinor: line.LineTotalMinor,
		NameSnapshot:   name,
		Modifiers:      rawModifiers,
		Note:           line.Note,
		SortOrder:      line.SortOrder,
	}
	if len(line.VariantName) > 0 {
		variantName, err := line.VariantName.Raw()
		if err != nil {
			return gen.OrderItem{}, fmt.Errorf("encode variant name: %w", err)
		}
		params.VariantNameSnapshot = variantName
	}

	return q.CreateOrderItem(ctx, params)
}

func isOrderCodeCollision(err error) bool   { return conflictOn(err, "order number") }
func isIdempotencyCollision(err error) bool { return conflictOn(err, "idempotency") }

func conflictOn(err error, marker string) bool {
	var statusErr *domain.StatusError
	return errors.Is(err, domain.ErrConflict) &&
		errors.As(err, &statusErr) &&
		strings.Contains(statusErr.Message, marker)
}

// catalog loads exactly the dishes the cart names, resolved into the lookups
// the pricer needs.
//
// Four queries by id rather than the public menu's six whole-restaurant scans:
// a two-line cart should not drag a four-hundred-item menu across the wire to
// be priced.
func (s *Order) catalog(ctx context.Context, restaurant gen.Restaurant, localNow time.Time, cart domain.Cart) (domain.Catalog, error) {
	itemIDs := make([]uuid.UUID, 0, len(cart.Lines))
	seen := make(map[uuid.UUID]struct{}, len(cart.Lines))
	for _, line := range cart.Lines {
		if _, ok := seen[line.ItemID]; ok {
			continue
		}
		seen[line.ItemID] = struct{}{}
		itemIDs = append(itemIDs, line.ItemID)
	}

	catalog := domain.Catalog{
		Currency: restaurant.Currency,
		Items:    make(map[uuid.UUID]*domain.CatalogItem, len(itemIDs)),
	}
	if len(itemIDs) == 0 {
		return catalog, nil
	}

	rows, err := s.store.ListOrderableItems(ctx, gen.ListOrderableItemsParams{
		RestaurantID: restaurant.ID, ItemIds: itemIDs,
	})
	if err != nil {
		return domain.Catalog{}, wrap("list orderable items", err)
	}

	found := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		menu, err := postgres.ToMenu(row.Menu)
		if err != nil {
			return domain.Catalog{}, fmt.Errorf("order catalog: %w", err)
		}
		item, err := postgres.ToItem(row.Item)
		if err != nil {
			return domain.Catalog{}, fmt.Errorf("order catalog: %w", err)
		}

		catalog.Items[item.ID] = &domain.CatalogItem{
			ID:          item.ID,
			Name:        item.Name,
			PriceMinor:  item.PriceMinor,
			IsAvailable: item.IsAvailable,
			// AvailableAt already covers menu.IsActive, so the category is the
			// only extra switch to consult here.
			Servable:        row.CategoryIsActive && menu.AvailableAt(localNow),
			Variants:        map[uuid.UUID]domain.CatalogVariant{},
			Groups:          map[uuid.UUID]*domain.CatalogGroup{},
			GroupOfModifier: map[uuid.UUID]uuid.UUID{},
		}
		found = append(found, item.ID)
	}

	// Nothing the cart named exists here. The pricer reports that per line,
	// which is a better message than a bare 404 on the whole order.
	if len(found) == 0 {
		return catalog, nil
	}

	variantRows, err := s.store.ListVariantsForItems(ctx, gen.ListVariantsForItemsParams{
		RestaurantID: restaurant.ID, ItemIds: found,
	})
	if err != nil {
		return domain.Catalog{}, wrap("list variants", err)
	}
	for _, v := range variantRows {
		item, ok := catalog.Items[v.ItemID]
		if !ok {
			continue
		}
		item.Variants[v.ID] = domain.CatalogVariant{
			ID:              v.ID,
			Name:            textOrEmpty(v.Name),
			PriceDeltaMinor: v.PriceDeltaMinor,
			IsAvailable:     v.IsAvailable,
		}
	}

	groupRows, err := s.store.ListModifierGroupsForItems(ctx, gen.ListModifierGroupsForItemsParams{
		RestaurantID: restaurant.ID, ItemIds: found,
	})
	if err != nil {
		return domain.Catalog{}, wrap("list modifier groups", err)
	}
	groupIDs := make([]uuid.UUID, 0, len(groupRows))
	groupOwner := make(map[uuid.UUID]*domain.CatalogItem, len(groupRows))
	for _, g := range groupRows {
		item, ok := catalog.Items[g.ItemID]
		if !ok {
			continue
		}
		item.Groups[g.ID] = &domain.CatalogGroup{
			ID:        g.ID,
			Name:      textOrEmpty(g.Name),
			MinSelect: g.MinSelect,
			MaxSelect: g.MaxSelect,
			Modifiers: map[uuid.UUID]domain.CatalogModifier{},
		}
		groupIDs = append(groupIDs, g.ID)
		groupOwner[g.ID] = item
	}

	if len(groupIDs) == 0 {
		return catalog, nil
	}

	modifierRows, err := s.store.ListModifiersForGroups(ctx, gen.ListModifiersForGroupsParams{
		RestaurantID: restaurant.ID, GroupIds: groupIDs,
	})
	if err != nil {
		return domain.Catalog{}, wrap("list modifiers", err)
	}
	for _, m := range modifierRows {
		item, ok := groupOwner[m.GroupID]
		if !ok {
			continue
		}
		item.Groups[m.GroupID].Modifiers[m.ID] = domain.CatalogModifier{
			ID:              m.ID,
			Name:            textOrEmpty(m.Name),
			PriceDeltaMinor: m.PriceDeltaMinor,
			IsAvailable:     m.IsAvailable,
		}
		item.GroupOfModifier[m.ID] = m.GroupID
	}

	return catalog, nil
}

// ------------------------------------------------------------------ guest reads

// GuestOrder returns one order to the guest who placed it.
//
// The session cookie is the only credential, and a mismatch is reported as "not
// found" rather than "forbidden": confirming that an id exists but belongs to
// someone else is itself information a stranger should not get.
func (s *Order) GuestOrder(ctx context.Context, id uuid.UUID, guestSessionID string) (domain.Order, error) {
	if !domain.ValidGuestSession(guestSessionID) {
		return domain.Order{}, domain.ErrNotFound
	}
	row, err := s.store.GetOrderForGuest(ctx, gen.GetOrderForGuestParams{
		ID: id, GuestSessionID: guestSessionID,
	})
	if err != nil {
		return domain.Order{}, wrap("get order", err)
	}
	lines, err := s.store.ListOrderItems(ctx, row.ID)
	if err != nil {
		return domain.Order{}, wrap("list order items", err)
	}
	return postgres.ToOrder(row, lines), nil
}

// GuestOrdersLimit is how far back "my orders" goes on the guest's phone. A
// guest cares about this meal, not their history.
const GuestOrdersLimit = 20

// GuestOrders lists what this guest ordered at this restaurant.
func (s *Order) GuestOrders(ctx context.Context, token, guestSessionID string) ([]domain.Order, error) {
	if !domain.ValidGuestSession(guestSessionID) {
		return []domain.Order{}, nil
	}
	token = domain.NormalizeQRToken(token)
	if !domain.ValidQRTokenShape(token) {
		return nil, domain.ErrNotFound
	}
	row, err := s.store.GetTableByToken(ctx, token)
	if err != nil {
		return nil, wrap("get table", err)
	}

	rows, err := s.store.ListGuestOrders(ctx, gen.ListGuestOrdersParams{
		GuestSessionID: guestSessionID,
		RestaurantID:   row.Restaurant.ID,
		RowLimit:       GuestOrdersLimit,
	})
	if err != nil {
		return nil, wrap("list guest orders", err)
	}
	return s.withLines(ctx, rows)
}

// ------------------------------------------------------------------ staff side

// AssertRestaurant checks that a staff member's tenant owns the restaurant they
// are asking about. The SSE handler calls it before subscribing, since a
// long-lived stream is exactly the wrong place to discover an authorisation
// mistake.
func (s *Order) AssertRestaurant(ctx context.Context, tenantID, restaurantID uuid.UUID) error {
	_, err := s.store.GetRestaurant(ctx, gen.GetRestaurantParams{ID: restaurantID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("restaurant: %w", postgres.MapError(err))
	}
	return nil
}

// OpenOrders is the kitchen screen's list, and what a client reloads after a
// reconnect or a resync hint.
func (s *Order) OpenOrders(ctx context.Context, tenantID, restaurantID uuid.UUID) ([]domain.Order, error) {
	if err := s.AssertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListOpenOrders(ctx, restaurantID)
	if err != nil {
		return nil, wrap("list open orders", err)
	}
	return s.withLines(ctx, rows)
}

// SetStatus moves an order through the kitchen.
//
// Three separate checks, and all three are load-bearing: the tenant owns the
// order, the role is allowed to set that status at all, and the move itself is
// legal from where the order currently is. The last one is then re-applied in
// the UPDATE's WHERE clause, so two devices tapping at the same moment cannot
// both succeed.
func (s *Order) SetStatus(ctx context.Context, tenantID, orderID uuid.UUID, role domain.Role, to domain.OrderStatus, reason string) (domain.Order, error) {
	if !to.Valid() {
		return domain.Order{}, domain.Errorf(domain.ErrNotFound, "%q is not an order status.", to)
	}

	row, err := s.store.GetOrderForTenant(ctx, gen.GetOrderForTenantParams{ID: orderID, TenantID: tenantID})
	if err != nil {
		return domain.Order{}, wrap("get order", err)
	}
	current := domain.OrderStatus(row.Order.Status)

	if !domain.RoleCanSetOrderStatus(role, to) {
		return domain.Order{}, domain.Errorf(domain.ErrForbidden,
			"Your role cannot mark an order as %s.", to)
	}
	if !current.CanTransition(to) {
		return domain.Order{}, domain.Errorf(domain.ErrConflict,
			"An order that is %s cannot become %s.", current, to)
	}

	params := gen.UpdateOrderStatusParams{
		Status:       string(to),
		ID:           orderID,
		RestaurantID: row.Order.RestaurantID,
		FromStatus:   string(current),
	}
	if reason = strings.TrimSpace(reason); reason != "" && to == domain.OrderCancelled {
		params.CancelReason = &reason
	}

	updated, err := s.store.UpdateOrderStatus(ctx, params)
	if err != nil {
		// No rows means the status changed between the read and the write —
		// someone else got there first. That is a conflict, not a missing
		// order, and saying so is what stops a tablet retrying forever.
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return domain.Order{}, domain.Errorf(domain.ErrConflict,
				"This order was updated by someone else. Reload and try again.")
		}
		return domain.Order{}, wrap("update order status", err)
	}

	lines, err := s.store.ListOrderItems(ctx, updated.ID)
	if err != nil {
		return domain.Order{}, wrap("list order items", err)
	}
	order := postgres.ToOrder(updated, lines)

	s.publish(ctx, order.RestaurantID, realtime.EventOrderUpdated, order.View())
	return order, nil
}

// ------------------------------------------------------------------ waiter calls

// CallWaiter raises a call from a table.
//
// A live call of the same kind is returned as-is rather than creating a second
// one. A guest tapping the button five times because nobody has come yet must
// not put five rows on the staff screen — that is the fastest way to make staff
// stop reading it.
func (s *Order) CallWaiter(ctx context.Context, token, guestSessionID string, kind domain.WaiterCallKind, note string) (Called, error) {
	if !kind.Valid() {
		return Called{}, domain.Errorf(domain.ErrNotFound, "%q is not a kind of call.", kind)
	}
	note, err := domain.ValidateCallNote(note)
	if err != nil {
		return Called{}, err
	}

	token = domain.NormalizeQRToken(token)
	if !domain.ValidQRTokenShape(token) {
		return Called{}, domain.ErrNotFound
	}
	row, err := s.store.GetTableByToken(ctx, token)
	if err != nil {
		return Called{}, wrap("get table", err)
	}
	table, restaurant := row.RestaurantTable, row.Restaurant

	existing, err := s.store.FindOpenWaiterCall(ctx, gen.FindOpenWaiterCallParams{
		TableID: &table.ID, Kind: string(kind),
	})
	if err == nil {
		return Called{Call: postgres.ToWaiterCall(existing)}, nil
	}
	if !errors.Is(postgres.MapError(err), domain.ErrNotFound) {
		return Called{}, wrap("find open waiter call", err)
	}

	params := gen.CreateWaiterCallParams{
		RestaurantID:   restaurant.ID,
		TableID:        &table.ID,
		TableLabel:     table.Label,
		Kind:           string(kind),
		GuestSessionID: guestSessionID,
	}
	if note != "" {
		params.Note = &note
	}

	created, err := s.store.CreateWaiterCall(ctx, params)
	if err != nil {
		mapped := postgres.MapError(err)
		// The partial unique index caught a call raised concurrently from the
		// same table; return that one, which is what the guest wanted anyway.
		if errors.Is(mapped, domain.ErrConflict) {
			if again, ferr := s.store.FindOpenWaiterCall(ctx, gen.FindOpenWaiterCallParams{
				TableID: &table.ID, Kind: string(kind),
			}); ferr == nil {
				return Called{Call: postgres.ToWaiterCall(again)}, nil
			}
		}
		return Called{}, fmt.Errorf("create waiter call: %w", mapped)
	}

	call := postgres.ToWaiterCall(created)
	// Published only on a genuinely new call. A repeat press changes nothing,
	// so pushing an event for it would make the staff screen flash for a call
	// that is already sitting on it.
	s.publish(ctx, restaurant.ID, realtime.EventWaiterCalled, call.View())
	return Called{Call: call, Created: true}, nil
}

func (s *Order) OpenCalls(ctx context.Context, tenantID, restaurantID uuid.UUID) ([]domain.WaiterCall, error) {
	if err := s.AssertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListOpenWaiterCalls(ctx, restaurantID)
	if err != nil {
		return nil, wrap("list waiter calls", err)
	}
	return postgres.ToWaiterCalls(rows), nil
}

func (s *Order) SetCallStatus(ctx context.Context, tenantID, callID uuid.UUID, to domain.WaiterCallStatus) (domain.WaiterCall, error) {
	if !to.Valid() {
		return domain.WaiterCall{}, domain.Errorf(domain.ErrNotFound, "%q is not a call status.", to)
	}

	row, err := s.store.GetWaiterCallForTenant(ctx, gen.GetWaiterCallForTenantParams{
		ID: callID, TenantID: tenantID,
	})
	if err != nil {
		return domain.WaiterCall{}, wrap("get waiter call", err)
	}
	current := domain.WaiterCallStatus(row.WaiterCall.Status)
	if !current.CanTransition(to) {
		return domain.WaiterCall{}, domain.Errorf(domain.ErrConflict,
			"A call that is %s cannot become %s.", current, to)
	}

	updated, err := s.store.UpdateWaiterCallStatus(ctx, gen.UpdateWaiterCallStatusParams{
		Status:       string(to),
		ID:           callID,
		RestaurantID: row.WaiterCall.RestaurantID,
		FromStatus:   string(current),
	})
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return domain.WaiterCall{}, domain.Errorf(domain.ErrConflict,
				"This call was updated by someone else. Reload and try again.")
		}
		return domain.WaiterCall{}, wrap("update waiter call", err)
	}

	call := postgres.ToWaiterCall(updated)
	s.publish(ctx, call.RestaurantID, realtime.EventWaiterUpdate, call.View())
	return call, nil
}

// ------------------------------------------------------------------ helpers

// withLines attaches every order's lines using a single second query.
func (s *Order) withLines(ctx context.Context, rows []gen.Order) ([]domain.Order, error) {
	if len(rows) == 0 {
		return []domain.Order{}, nil
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	lines, err := s.store.ListOrderItemsForOrders(ctx, ids)
	if err != nil {
		return nil, wrap("list order items", err)
	}
	return postgres.ToOrders(rows, lines), nil
}

// publish pushes an event to the staff screens.
//
// A failure here is logged and swallowed on purpose: the order is already
// committed, and turning "the tablet did not hear about it" into a failed
// request would tell the guest their order was rejected when it was not. The
// staff screen reloads on reconnect, so a missed event costs a refresh.
func (s *Order) publish(ctx context.Context, restaurantID uuid.UUID, eventType string, payload any) {
	if s.hub == nil {
		return
	}
	if err := s.hub.Publish(restaurantID, eventType, payload); err != nil {
		s.logger.WarnContext(ctx, "could not publish staff event",
			slog.String("event", eventType), slog.Any("error", err))
	}
}

func textOrEmpty(raw []byte) i18n.Text {
	t, err := i18n.FromRaw(raw)
	if err != nil {
		return i18n.Text{}
	}
	return t
}
