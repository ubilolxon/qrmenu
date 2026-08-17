package service_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/realtime"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// Ordering is the first surface an unauthenticated stranger can write through,
// so these run against a real Postgres: the guarantees being checked live in
// composite foreign keys, partial unique indexes and a compare-and-set UPDATE,
// none of which a fake repository would reproduce.

func (h *harness) orderService(t *testing.T) (*service.Order, *realtime.Hub) {
	t.Helper()
	hub := realtime.New()
	return service.NewOrder(postgres.NewStore(h.pool), hub, slog.New(slog.DiscardHandler)), hub
}

// diner is a restaurant with one orderable dish and a table to order it from.
type diner struct {
	restaurantID uuid.UUID
	item         domain.Item
	table        domain.Table
}

func (h *harness) diner(t *testing.T, tenantID uuid.UUID) diner {
	t.Helper()
	ctx := context.Background()

	built := h.tree(t, tenantID)
	table, err := service.NewTable(postgres.NewStore(h.pool)).
		Create(ctx, tenantID, built.restaurantID, domain.TableInput{Label: "Stol " + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return diner{restaurantID: built.restaurantID, item: built.item, table: table}
}

func mustSession(t *testing.T) string {
	t.Helper()
	session, err := domain.NewGuestSession()
	if err != nil {
		t.Fatalf("new guest session: %v", err)
	}
	return session
}

func TestPlaceOrderRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, hub := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)
	guest := mustSession(t)

	// A staff device is watching before the order is placed, which is the only
	// way to prove the event actually fires on the write path.
	sub := hub.Subscribe(place.restaurantID)
	defer hub.Unsubscribe(sub)

	placed, err := orders.Place(ctx, place.table.QRToken, guest, service.PlaceInput{
		Cart:   domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 3}}},
		Locale: "ru",
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}
	order := placed.Order

	if placed.Replayed {
		t.Error("a first order must not be reported as a replay")
	}
	if order.Status != domain.OrderPending {
		t.Errorf("status = %q, want pending", order.Status)
	}
	// The price came from the database, not the request.
	if want := place.item.PriceMinor * 3; order.TotalMinor != want {
		t.Errorf("total = %d, want %d", order.TotalMinor, want)
	}
	if order.Currency != "UZS" {
		t.Errorf("currency = %q, want the restaurant's own", order.Currency)
	}
	if order.CodeSeq < 1 {
		t.Errorf("code sequence = %d, want a positive daily number", order.CodeSeq)
	}
	// The snapshot is what makes an order a record rather than a view.
	if len(order.Lines) != 1 || order.Lines[0].Name["ru"] != "Ачичук" {
		t.Errorf("line name snapshot missing: %+v", order.Lines)
	}
	if order.TableLabel != place.table.Label {
		t.Errorf("table label snapshot = %q, want %q", order.TableLabel, place.table.Label)
	}

	select {
	case event := <-sub.Events():
		if event.Type != realtime.EventOrderCreated {
			t.Errorf("event type = %q, want %q", event.Type, realtime.EventOrderCreated)
		}
	default:
		t.Error("placing an order must push an event to the staff stream")
	}

	// The guest can read their own order back with nothing but the cookie.
	fetched, err := orders.GuestOrder(ctx, order.ID, guest)
	if err != nil {
		t.Fatalf("guest read back: %v", err)
	}
	if fetched.ID != order.ID {
		t.Errorf("read back the wrong order")
	}
}

// The guest session is the only credential in the ordering flow, so it has to
// behave like one.
func TestGuestCannotReadAnotherGuestsOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)

	placed, err := orders.Place(ctx, place.table.QRToken, mustSession(t), service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}},
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}

	// Another diner at the same table, with a valid session of their own.
	if _, err := orders.GuestOrder(ctx, placed.Order.ID, mustSession(t)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a stranger's session got %v, want ErrNotFound", err)
	}
	// And with no session at all.
	if _, err := orders.GuestOrder(ctx, placed.Order.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("an empty session got %v, want ErrNotFound", err)
	}
}

// A phone on bad wifi retries. The retry must not put a second ticket in the
// kitchen.
func TestIdempotentPlaceReturnsTheSameOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)
	guest := mustSession(t)

	in := service.PlaceInput{
		Cart:           domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}},
		IdempotencyKey: "retry-" + uuid.NewString(),
	}

	first, err := orders.Place(ctx, place.table.QRToken, guest, in)
	if err != nil {
		t.Fatalf("first place: %v", err)
	}
	second, err := orders.Place(ctx, place.table.QRToken, guest, in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if second.Order.ID != first.Order.ID {
		t.Fatalf("retry created a second order: %s then %s", first.Order.ID, second.Order.ID)
	}
	if !second.Replayed {
		t.Error("the retry should be reported as a replay so the client answers 200, not 201")
	}

	// Without a key, the same cart is a genuinely new order — someone ordering
	// another round is not a duplicate.
	third, err := orders.Place(ctx, place.table.QRToken, guest, service.PlaceInput{Cart: in.Cart})
	if err != nil {
		t.Fatalf("third place: %v", err)
	}
	if third.Order.ID == first.Order.ID {
		t.Error("an order placed without an idempotency key must be a new order")
	}
	if third.Order.CodeSeq == first.Order.CodeSeq {
		t.Errorf("two orders in one day share the number %d", third.Order.CodeSeq)
	}
}

// A cart may only name dishes from the restaurant whose table it was scanned
// at. This is the cross-tenant case, and the message must not confirm that the
// id exists somewhere else on the platform.
func TestCannotOrderAnotherRestaurantsDish(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	mine := h.tenant(t)
	theirs := h.tenant(t)
	myPlace := h.diner(t, mine.User.TenantID)
	theirPlace := h.diner(t, theirs.User.TenantID)

	_, err := orders.Place(ctx, myPlace.table.QRToken, mustSession(t), service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: theirPlace.item.ID, Qty: 1}}},
	})

	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected the dish to be rejected, got %v", err)
	}
	if validationErr.Fields[0].Field != "lines[0].item_id" {
		t.Errorf("field = %q, want lines[0].item_id", validationErr.Fields[0].Field)
	}
}

func TestStaffCannotTouchAnotherTenantsOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	mine := h.tenant(t)
	theirs := h.tenant(t)
	myPlace := h.diner(t, mine.User.TenantID)

	placed, err := orders.Place(ctx, myPlace.table.QRToken, mustSession(t), service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: myPlace.item.ID, Qty: 1}}},
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}

	// The other tenant's owner, with a real id in hand.
	if _, err := orders.SetStatus(ctx, theirs.User.TenantID, placed.Order.ID,
		domain.RoleOwner, domain.OrderAccepted, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign tenant SetStatus got %v, want ErrNotFound", err)
	}
	if _, err := orders.OpenOrders(ctx, theirs.User.TenantID, myPlace.restaurantID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign tenant OpenOrders got %v, want ErrNotFound", err)
	}
	if err := orders.AssertRestaurant(ctx, theirs.User.TenantID, myPlace.restaurantID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign tenant AssertRestaurant got %v, want ErrNotFound", err)
	}

	// And the rightful owner still can.
	if _, err := orders.SetStatus(ctx, mine.User.TenantID, placed.Order.ID,
		domain.RoleOwner, domain.OrderAccepted, ""); err != nil {
		t.Errorf("the owning tenant should be able to accept: %v", err)
	}
}

func TestOrderStatusMachineIsEnforcedInTheDatabase(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, hub := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)
	tenantID := owner.User.TenantID

	placed, err := orders.Place(ctx, place.table.QRToken, mustSession(t), service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}},
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}
	id := placed.Order.ID

	// Skipping ahead is refused.
	if _, err := orders.SetStatus(ctx, tenantID, id, domain.RoleOwner, domain.OrderClosed, ""); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("pending -> closed got %v, want ErrConflict", err)
	}

	sub := hub.Subscribe(place.restaurantID)
	defer hub.Unsubscribe(sub)

	accepted, err := orders.SetStatus(ctx, tenantID, id, domain.RoleWaiter, domain.OrderAccepted, "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.AcceptedAt == nil {
		t.Error("accepted_at should be stamped when an order is accepted")
	}
	select {
	case event := <-sub.Events():
		if event.Type != realtime.EventOrderUpdated {
			t.Errorf("event = %q, want %q", event.Type, realtime.EventOrderUpdated)
		}
	default:
		t.Error("a status change must reach the staff stream")
	}

	// The role check is separate from the transition check: preparing is a
	// legal move from accepted, but not one a waiter makes.
	if _, err := orders.SetStatus(ctx, tenantID, id, domain.RoleWaiter, domain.OrderPreparing, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("waiter -> preparing got %v, want ErrForbidden", err)
	}
	if _, err := orders.SetStatus(ctx, tenantID, id, domain.RoleKitchen, domain.OrderPreparing, ""); err != nil {
		t.Fatalf("kitchen -> preparing: %v", err)
	}

	ready, err := orders.SetStatus(ctx, tenantID, id, domain.RoleKitchen, domain.OrderReady, "")
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if ready.ReadyAt == nil {
		t.Error("ready_at should be stamped")
	}
	// Backwards, from a role that is otherwise allowed to set 'preparing'.
	if _, err := orders.SetStatus(ctx, tenantID, id, domain.RoleKitchen, domain.OrderPreparing, ""); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("ready -> preparing got %v, want ErrConflict; an order must never go backwards", err)
	}

	if _, err := orders.SetStatus(ctx, tenantID, id, domain.RoleWaiter, domain.OrderServed, ""); err != nil {
		t.Fatalf("served: %v", err)
	}
	// Food already on the table cannot be cancelled.
	if _, err := orders.SetStatus(ctx, tenantID, id, domain.RoleOwner, domain.OrderCancelled, "changed mind"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("served -> cancelled got %v, want ErrConflict", err)
	}

	closed, err := orders.SetStatus(ctx, tenantID, id, domain.RoleWaiter, domain.OrderClosed, "")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.ClosedAt == nil {
		t.Error("closed_at should be stamped")
	}
	// A closed order has left the board.
	open, err := orders.OpenOrders(ctx, tenantID, place.restaurantID)
	if err != nil {
		t.Fatalf("open orders: %v", err)
	}
	for _, o := range open {
		if o.ID == id {
			t.Error("a closed order must not stay on the kitchen board")
		}
	}
}

func TestCancelRecordsItsReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)

	placed, err := orders.Place(ctx, place.table.QRToken, mustSession(t), service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}},
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}

	cancelled, err := orders.SetStatus(ctx, owner.User.TenantID, placed.Order.ID,
		domain.RoleOwner, domain.OrderCancelled, "kitchen ran out")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.CancelReason == nil || *cancelled.CancelReason != "kitchen ran out" {
		t.Errorf("cancel reason = %v, want it recorded", cancelled.CancelReason)
	}
}

// A guest holding the button down must not fill the staff screen.
func TestWaiterCallIsDeduplicated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)
	guest := mustSession(t)

	first, err := orders.CallWaiter(ctx, place.table.QRToken, guest, domain.CallWaiter, "")
	if err != nil {
		t.Fatalf("call waiter: %v", err)
	}
	if !first.Created {
		t.Error("the first press should create a call")
	}
	second, err := orders.CallWaiter(ctx, place.table.QRToken, guest, domain.CallWaiter, "")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.Call.ID != first.Call.ID {
		t.Error("a repeated call while one is still live must return the existing call")
	}
	if second.Created {
		t.Error("a deduped press must not report a new call, or the handler answers 201 for a row it did not create")
	}

	// A different kind is a different request and gets its own row.
	bill, err := orders.CallWaiter(ctx, place.table.QRToken, guest, domain.CallBill, "")
	if err != nil {
		t.Fatalf("call for the bill: %v", err)
	}
	if bill.Call.ID == first.Call.ID {
		t.Error("asking for the bill is not the same as asking for a waiter")
	}

	open, err := orders.OpenCalls(ctx, owner.User.TenantID, place.restaurantID)
	if err != nil {
		t.Fatalf("open calls: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("open calls = %d, want 2", len(open))
	}

	// Once resolved, the button works again.
	if _, err := orders.SetCallStatus(ctx, owner.User.TenantID, first.Call.ID, domain.CallResolved); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	third, err := orders.CallWaiter(ctx, place.table.QRToken, guest, domain.CallWaiter, "")
	if err != nil {
		t.Fatalf("call after resolve: %v", err)
	}
	if third.Call.ID == first.Call.ID {
		t.Error("after a call is resolved, the next press should raise a new one")
	}
}

func TestStaffCannotTouchAnotherTenantsCall(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	mine := h.tenant(t)
	theirs := h.tenant(t)
	place := h.diner(t, mine.User.TenantID)

	call, err := orders.CallWaiter(ctx, place.table.QRToken, mustSession(t), domain.CallWaiter, "")
	if err != nil {
		t.Fatalf("call waiter: %v", err)
	}

	if _, err := orders.SetCallStatus(ctx, theirs.User.TenantID, call.Call.ID, domain.CallResolved); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign tenant got %v, want ErrNotFound", err)
	}
	if _, err := orders.OpenCalls(ctx, theirs.User.TenantID, place.restaurantID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign tenant OpenCalls got %v, want ErrNotFound", err)
	}
}

// Deleting the dish or the table must not delete the history, and must not
// leave the ticket unreadable.
func TestOrderHistorySurvivesTheMenuChanging(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	tenantID := owner.User.TenantID
	place := h.diner(t, tenantID)
	guest := mustSession(t)

	placed, err := orders.Place(ctx, place.table.QRToken, guest, service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 2}}},
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}
	originalTotal := placed.Order.TotalMinor

	// The owner reprices the dish and then deletes it outright.
	newPrice := place.item.PriceMinor * 10
	if _, err := h.menus.UpdateItem(ctx, tenantID, place.restaurantID, place.item.ID,
		service.ItemPatch{PriceMinor: &newPrice}); err != nil {
		t.Fatalf("reprice: %v", err)
	}
	if err := h.menus.DeleteItem(ctx, tenantID, place.restaurantID, place.item.ID); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	after, err := orders.GuestOrder(ctx, placed.Order.ID, guest)
	if err != nil {
		t.Fatalf("read order after the menu changed: %v", err)
	}
	if after.TotalMinor != originalTotal {
		t.Errorf("total changed from %d to %d; an order must record what the guest actually agreed to pay",
			originalTotal, after.TotalMinor)
	}
	if len(after.Lines) != 1 {
		t.Fatalf("lines = %d, want the line to survive the dish being deleted", len(after.Lines))
	}
	if after.Lines[0].Name["ru"] != "Ачичук" {
		t.Errorf("name snapshot lost: %v", after.Lines[0].Name)
	}
	// The pointer is nulled, the snapshot is not — which is exactly the split
	// the composite ON DELETE SET NULL (item_id) exists to produce.
	if after.Lines[0].ItemID != nil {
		t.Error("item_id should be null once the dish is gone")
	}
}

func TestDeletingATableKeepsItsOrders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	tenantID := owner.User.TenantID
	place := h.diner(t, tenantID)
	guest := mustSession(t)

	placed, err := orders.Place(ctx, place.table.QRToken, guest, service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}},
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}

	tables := service.NewTable(postgres.NewStore(h.pool))
	if err := tables.Delete(ctx, tenantID, place.restaurantID, place.table.ID); err != nil {
		t.Fatalf("delete table: %v", err)
	}

	after, err := orders.GuestOrder(ctx, placed.Order.ID, guest)
	if err != nil {
		t.Fatalf("read order after the table was deleted: %v", err)
	}
	if after.TableID != nil {
		t.Error("table_id should be null once the table is gone")
	}
	if after.TableLabel != place.table.Label {
		t.Errorf("table label = %q, want the snapshot %q", after.TableLabel, place.table.Label)
	}
}

// A sold-out dish must be refused at order time, not just hidden on the page —
// the page is cached and the guest's cart may be minutes old.
func TestCannotOrderASoldOutDish(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)

	if _, err := h.menus.SetItemAvailability(ctx, owner.User.TenantID, place.restaurantID, place.item.ID, false); err != nil {
		t.Fatalf("mark sold out: %v", err)
	}

	_, err := orders.Place(ctx, place.table.QRToken, mustSession(t), service.PlaceInput{
		Cart: domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}},
	})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected a sold-out dish to be refused, got %v", err)
	}
}

func TestPlaceRejectsAnUnknownTable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orders, _ := h.orderService(t)

	owner := h.tenant(t)
	place := h.diner(t, owner.User.TenantID)

	cart := domain.Cart{Lines: []domain.CartLine{{ItemID: place.item.ID, Qty: 1}}}

	// A token that is well-formed but belongs to nothing.
	token, err := domain.NewQRToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := orders.Place(ctx, token, mustSession(t), service.PlaceInput{Cart: cart}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown token got %v, want ErrNotFound", err)
	}
	// And one that could never exist, which must not reach the database at all.
	if _, err := orders.Place(ctx, "!!!", mustSession(t), service.PlaceInput{Cart: cart}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("malformed token got %v, want ErrNotFound", err)
	}
}
