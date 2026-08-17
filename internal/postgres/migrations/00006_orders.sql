-- +goose Up

-- item_variants was the one node of the menu tree without a (id, restaurant_id)
-- key, because nothing referenced it compositely yet. order_items does, so the
-- constraint is added here rather than leaving that one edge unguarded.
ALTER TABLE item_variants
    ADD CONSTRAINT item_variants_id_restaurant_uniq UNIQUE (id, restaurant_id);

-- restaurant_tables needs the same composite target, for the same reason:
-- orders and waiter_calls both reference (id, restaurant_id) so a row can never
-- point at another restaurant's table. Both ALTERs run before the tables that
-- depend on them — a composite foreign key needs its target constraint to
-- already exist.
ALTER TABLE restaurant_tables
    ADD CONSTRAINT restaurant_tables_id_restaurant_uniq UNIQUE (id, restaurant_id);

-- An order placed by a guest from a table.
--
-- Almost every column here exists because an order is a *historical record*,
-- not a view over the current menu. The dish it names may be renamed, repriced
-- or deleted tomorrow; the table it came from may be relabelled or removed. A
-- receipt that changes retroactively is not a receipt, so everything the ticket
-- and the history need is snapshotted at the moment the order is placed.
CREATE TABLE orders (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,

    -- Nullable so deleting a table does not delete its order history. The
    -- column list after SET NULL matters for the same reason it did on images:
    -- an unqualified SET NULL on a composite key nulls restaurant_id too, and
    -- that column is NOT NULL.
    table_id uuid,
    -- Snapshot, so a ticket still says "Stol 5" after the table is renamed or
    -- removed. This is the label staff shout across the room; it must not
    -- become "unknown table" because someone tidied the floor plan.
    table_label text NOT NULL,

    -- The number a waiter calls out. It restarts every day, per restaurant,
    -- which is what everyone in a dining room already expects — "order 42"
    -- means today's 42nd. business_day is the restaurant's own calendar day in
    -- its own timezone, not UTC's: service that runs past midnight belongs to
    -- the day it started, and a restaurant in Tashkent must not roll over at
    -- 05:00 local because the server thinks in UTC.
    --
    -- Only the sequence is stored. The display form ("042") is derived, so the
    -- two can never drift apart.
    code_seq     integer NOT NULL CHECK (code_seq > 0),
    business_day date NOT NULL,

    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'accepted', 'preparing', 'ready', 'served', 'closed', 'cancelled')),

    -- Opaque random id from the guest's cookie. It is the only thing tying an
    -- anonymous person to the order they just placed, and therefore the only
    -- thing that authorises reading it back. Never guessable, never derived
    -- from anything the guest could be identified by.
    guest_session_id text NOT NULL CHECK (length(guest_session_id) BETWEEN 16 AND 128),

    -- The language the guest was reading. The kitchen ticket renders in the
    -- restaurant's default locale, but a "your order" screen should come back
    -- in the language it was placed in.
    locale text NOT NULL,

    note text CHECK (note IS NULL OR length(note) <= 500),

    -- Every one of these is computed by the server from the current menu. The
    -- client sends ids and quantities and nothing else; a total that arrives in
    -- a request body is how you get a 0-som order.
    subtotal_minor    bigint NOT NULL CHECK (subtotal_minor >= 0),
    -- Reserved: there is no fee policy yet, so this is always 0. It exists now
    -- because total_minor must be a stored number either way, and adding the
    -- component later would mean backfilling every historical row to keep
    -- subtotal + fee = total true.
    service_fee_minor bigint NOT NULL DEFAULT 0 CHECK (service_fee_minor >= 0),
    total_minor       bigint NOT NULL CHECK (total_minor >= 0),
    -- Snapshot too: a restaurant that switches currency must not reprice its
    -- own history.
    currency text NOT NULL,

    -- A phone on restaurant wifi will double-tap and lose signal mid-request.
    -- The retry carries the same key and gets the same order back instead of a
    -- second one arriving in the kitchen.
    idempotency_key text CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 8 AND 200),

    placed_at   timestamptz NOT NULL DEFAULT now(),
    accepted_at timestamptz,
    ready_at    timestamptz,
    closed_at   timestamptz,
    cancel_reason text CHECK (cancel_reason IS NULL OR length(cancel_reason) <= 300),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT orders_total_consistent CHECK (total_minor = subtotal_minor + service_fee_minor),
    CONSTRAINT orders_table_fk FOREIGN KEY (table_id, restaurant_id)
        REFERENCES restaurant_tables (id, restaurant_id) ON DELETE SET NULL (table_id),
    -- So order_items can carry restaurant_id compositely, exactly as the menu
    -- tree does: a line item physically cannot be attached to another
    -- restaurant's order.
    CONSTRAINT orders_id_restaurant_uniq UNIQUE (id, restaurant_id)
);

-- Two orders numbered 42 on the same day in the same restaurant would make the
-- number useless for the one thing it exists for. The service allocates
-- max+1 and retries on a collision, which this index is what makes safe.
CREATE UNIQUE INDEX orders_code_uniq ON orders (restaurant_id, business_day, code_seq);

-- Idempotency is scoped to the table, not globally: two guests at different
-- tables picking the same key is not a replay.
CREATE UNIQUE INDEX orders_idempotency_uniq
    ON orders (table_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND table_id IS NOT NULL;

-- The kitchen screen's only query: everything still in play, newest first.
CREATE INDEX orders_open_idx
    ON orders (restaurant_id, placed_at DESC)
    WHERE status NOT IN ('closed', 'cancelled');

CREATE INDEX orders_restaurant_day_idx ON orders (restaurant_id, business_day, placed_at DESC);

-- "Show me my orders" from the guest's phone.
CREATE INDEX orders_guest_idx ON orders (guest_session_id, placed_at DESC);

CREATE TRIGGER orders_set_updated_at
    BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One line of an order.
--
-- item_id and variant_id are pointers for reporting ("how many plovs this
-- month"), not for rendering. Everything rendered comes from the snapshots, so
-- a deleted dish still prints correctly on a six-month-old receipt.
CREATE TABLE order_items (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    order_id      uuid NOT NULL,
    restaurant_id uuid NOT NULL,

    item_id    uuid,
    variant_id uuid,

    qty              integer NOT NULL CHECK (qty BETWEEN 1 AND 99),
    -- Item price + variant delta + every selected modifier delta, for one unit.
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor >= 0),
    line_total_minor bigint NOT NULL CHECK (line_total_minor >= 0),

    -- The full translation object, not one resolved string: the guest ordered
    -- in Russian, the kitchen reads Uzbek, and the owner's report is in English.
    name_snapshot         jsonb NOT NULL,
    variant_name_snapshot jsonb,
    -- [{"id":..., "name": {...}, "price_delta_minor": ...}, ...]. JSONB rather
    -- than a third table: written once, always read as a set, never queried
    -- individually.
    modifiers jsonb NOT NULL DEFAULT '[]'::jsonb,

    note       text CHECK (note IS NULL OR length(note) <= 300),
    sort_order integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT order_items_line_total CHECK (line_total_minor = unit_price_minor * qty),
    CONSTRAINT order_items_order_fk FOREIGN KEY (order_id, restaurant_id)
        REFERENCES orders (id, restaurant_id) ON DELETE CASCADE,
    CONSTRAINT order_items_item_fk FOREIGN KEY (item_id, restaurant_id)
        REFERENCES items (id, restaurant_id) ON DELETE SET NULL (item_id),
    CONSTRAINT order_items_variant_fk FOREIGN KEY (variant_id, restaurant_id)
        REFERENCES item_variants (id, restaurant_id) ON DELETE SET NULL (variant_id)
);

CREATE INDEX order_items_order_idx ON order_items (order_id, sort_order);
CREATE INDEX order_items_item_idx ON order_items (item_id) WHERE item_id IS NOT NULL;

-- "Waiter, please" from the table, without anyone waving.
CREATE TABLE waiter_calls (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,

    table_id    uuid,
    table_label text NOT NULL,

    kind   text NOT NULL CHECK (kind IN ('waiter', 'bill', 'help')),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'acknowledged', 'resolved')),

    guest_session_id text NOT NULL,
    note text CHECK (note IS NULL OR length(note) <= 300),

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    acknowledged_at timestamptz,
    resolved_at     timestamptz,

    CONSTRAINT waiter_calls_table_fk FOREIGN KEY (table_id, restaurant_id)
        REFERENCES restaurant_tables (id, restaurant_id) ON DELETE SET NULL (table_id)
);

-- One live call per table per kind. A guest tapping the button five times
-- while nobody comes must not put five rows on the staff screen — that is the
-- fastest way to make staff stop reading it. Acknowledged still counts as
-- live, so the button stays inert until the call is actually resolved.
CREATE UNIQUE INDEX waiter_calls_open_uniq
    ON waiter_calls (table_id, kind)
    WHERE status <> 'resolved' AND table_id IS NOT NULL;

CREATE INDEX waiter_calls_open_idx
    ON waiter_calls (restaurant_id, created_at DESC)
    WHERE status <> 'resolved';

CREATE TRIGGER waiter_calls_set_updated_at
    BEFORE UPDATE ON waiter_calls FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS waiter_calls;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;

ALTER TABLE restaurant_tables DROP CONSTRAINT IF EXISTS restaurant_tables_id_restaurant_uniq;
ALTER TABLE item_variants DROP CONSTRAINT IF EXISTS item_variants_id_restaurant_uniq;
