-- Orders, their lines, and the waiter-call button.

-- Allocates the next daily order number, and reports the restaurant's own
-- clock in the same round trip.
--
-- Both come from Postgres rather than Go on purpose. The business day depends
-- on the restaurant's timezone, and Postgres carries the full tzdata while a
-- scratch container running the API may carry none at all — a missing zone in
-- Go silently degrades to UTC, which would roll the order numbers over at
-- 05:00 in Tashkent. local_now rides along because the menu serving-window
-- check needs the same clock, and taking it from a second source would let the
-- two disagree.
--
-- The count is scoped to today, so the number restarts every morning. Two
-- orders can still race to the same value; orders_code_uniq is what makes that
-- safe, and the service retries.
--
-- name: NextOrderCode :one
SELECT
    (now() AT TIME ZONE sqlc.arg(timezone)::text)::date      AS business_day,
    (now() AT TIME ZONE sqlc.arg(timezone)::text)::timestamp AS local_now,
    (coalesce(max(o.code_seq), 0) + 1)::integer              AS next_seq
FROM orders o
WHERE o.restaurant_id = sqlc.arg(restaurant_id)
  AND o.business_day = (now() AT TIME ZONE sqlc.arg(timezone)::text)::date;

-- name: CreateOrder :one
INSERT INTO orders (
    restaurant_id, table_id, table_label, code_seq, business_day,
    guest_session_id, locale, note,
    subtotal_minor, service_fee_minor, total_minor, currency,
    idempotency_key
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: CreateOrderItem :one
INSERT INTO order_items (
    order_id, restaurant_id, item_id, variant_id, qty,
    unit_price_minor, line_total_minor,
    name_snapshot, variant_name_snapshot, modifiers, note, sort_order
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- The replay lookup. Scoped to the table, because two guests at different
-- tables choosing the same key is a coincidence, not a retry.
--
-- name: GetOrderByIdempotencyKey :one
SELECT * FROM orders
WHERE table_id = $1 AND idempotency_key = $2;

-- name: GetOrder :one
SELECT * FROM orders WHERE id = $1 AND restaurant_id = $2;

-- Staff reach an order by its id alone, so the tenant check has to happen in
-- the query. Joining through restaurants is what stops one tenant's waiter
-- patching another tenant's order by pasting an id.
--
-- name: GetOrderForTenant :one
SELECT sqlc.embed(o), sqlc.embed(r)
FROM orders o
JOIN restaurants r ON r.id = o.restaurant_id
WHERE o.id = sqlc.arg(id) AND r.tenant_id = sqlc.arg(tenant_id);

-- A guest's own order. The session id is the credential: without it there is
-- nothing to check, since the guest never logged in.
--
-- name: GetOrderForGuest :one
SELECT * FROM orders WHERE id = $1 AND guest_session_id = $2;

-- name: ListOrderItems :many
SELECT * FROM order_items WHERE order_id = $1 ORDER BY sort_order, created_at;

-- Lines for a whole screenful of orders in one query, so listing N orders is
-- two round trips instead of N+1.
--
-- name: ListOrderItemsForOrders :many
SELECT * FROM order_items
WHERE order_id = ANY(sqlc.arg(order_ids)::uuid[])
ORDER BY order_id, sort_order, created_at;

-- The kitchen screen. Oldest first: the order that has been waiting longest is
-- the one that needs attention, which is the opposite of every other list in
-- the product.
--
-- name: ListOpenOrders :many
SELECT * FROM orders
WHERE restaurant_id = $1 AND status NOT IN ('closed', 'cancelled')
ORDER BY placed_at;

-- name: ListOrdersByDay :many
SELECT * FROM orders
WHERE restaurant_id = sqlc.arg(restaurant_id)
  AND business_day = sqlc.arg(business_day)
ORDER BY placed_at DESC;

-- name: ListGuestOrders :many
SELECT * FROM orders
WHERE guest_session_id = sqlc.arg(guest_session_id)
  AND restaurant_id = sqlc.arg(restaurant_id)
ORDER BY placed_at DESC
LIMIT sqlc.arg(row_limit);

-- Compare-and-set on the status.
--
-- The WHERE clause pins the status the caller believed the order was in, so two
-- waiters tapping "ready" at the same moment produce one update and one
-- no-rows, rather than both succeeding and the second silently overwriting a
-- decision made in between. Checking the transition in Go and then updating
-- unconditionally would leave exactly that window open.
--
-- The timestamps are set once and never overwritten: accepted_at should say
-- when the order was first taken on, not when someone last touched the row.
--
-- name: UpdateOrderStatus :one
UPDATE orders
SET status = sqlc.arg(status)::text,
    accepted_at = CASE WHEN sqlc.arg(status)::text = 'accepted' AND accepted_at IS NULL
                       THEN now() ELSE accepted_at END,
    ready_at    = CASE WHEN sqlc.arg(status)::text = 'ready' AND ready_at IS NULL
                       THEN now() ELSE ready_at END,
    closed_at   = CASE WHEN sqlc.arg(status)::text IN ('closed', 'cancelled') AND closed_at IS NULL
                       THEN now() ELSE closed_at END,
    cancel_reason = COALESCE(sqlc.narg(cancel_reason)::text, cancel_reason)
WHERE id = sqlc.arg(id)
  AND restaurant_id = sqlc.arg(restaurant_id)
  AND status = sqlc.arg(from_status)::text
RETURNING *;

-- ------------------------------------------------------------------ catalog
--
-- The order path reads the menu by id rather than reusing the public menu's
-- whole-restaurant scans: a two-line cart should not pull a four-hundred-item
-- menu through the connection to price it.

-- Brings the category and menu along, because whether a dish can be ordered
-- depends on both — a dish in a switched-off category, or on a menu outside its
-- serving window, is not orderable no matter what the item row says.
--
-- name: ListOrderableItems :many
SELECT sqlc.embed(i), c.is_active AS category_is_active, sqlc.embed(m)
FROM items i
JOIN categories c ON c.id = i.category_id AND c.restaurant_id = i.restaurant_id
JOIN menus m ON m.id = c.menu_id AND m.restaurant_id = c.restaurant_id
WHERE i.restaurant_id = sqlc.arg(restaurant_id)
  AND i.id = ANY(sqlc.arg(item_ids)::uuid[]);

-- name: ListVariantsForItems :many
SELECT * FROM item_variants
WHERE restaurant_id = sqlc.arg(restaurant_id)
  AND item_id = ANY(sqlc.arg(item_ids)::uuid[])
ORDER BY sort_order, created_at;

-- name: ListModifierGroupsForItems :many
SELECT * FROM modifier_groups
WHERE restaurant_id = sqlc.arg(restaurant_id)
  AND item_id = ANY(sqlc.arg(item_ids)::uuid[])
ORDER BY sort_order, created_at;

-- name: ListModifiersForGroups :many
SELECT * FROM modifiers
WHERE restaurant_id = sqlc.arg(restaurant_id)
  AND group_id = ANY(sqlc.arg(group_ids)::uuid[])
ORDER BY sort_order, created_at;

-- ------------------------------------------------------------- waiter calls

-- name: CreateWaiterCall :one
INSERT INTO waiter_calls (restaurant_id, table_id, table_label, kind, guest_session_id, note)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- The dedupe lookup: a guest pressing the button again while the first call is
-- still live gets that call back instead of adding a second row to the screen.
--
-- name: FindOpenWaiterCall :one
SELECT * FROM waiter_calls
WHERE table_id = $1 AND kind = $2 AND status <> 'resolved';

-- name: GetWaiterCall :one
SELECT * FROM waiter_calls WHERE id = $1 AND restaurant_id = $2;

-- name: GetWaiterCallForTenant :one
SELECT sqlc.embed(w), sqlc.embed(r)
FROM waiter_calls w
JOIN restaurants r ON r.id = w.restaurant_id
WHERE w.id = sqlc.arg(id) AND r.tenant_id = sqlc.arg(tenant_id);

-- name: ListOpenWaiterCalls :many
SELECT * FROM waiter_calls
WHERE restaurant_id = $1 AND status <> 'resolved'
ORDER BY created_at;

-- Same compare-and-set shape as orders, for the same reason.
--
-- name: UpdateWaiterCallStatus :one
UPDATE waiter_calls
SET status = sqlc.arg(status)::text,
    acknowledged_at = CASE WHEN sqlc.arg(status)::text = 'acknowledged' AND acknowledged_at IS NULL
                           THEN now() ELSE acknowledged_at END,
    resolved_at     = CASE WHEN sqlc.arg(status)::text = 'resolved' AND resolved_at IS NULL
                           THEN now() ELSE resolved_at END
WHERE id = sqlc.arg(id)
  AND restaurant_id = sqlc.arg(restaurant_id)
  AND status = sqlc.arg(from_status)::text
RETURNING *;
