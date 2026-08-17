-- +goose Up

-- A physical table in the restaurant, and the QR code stuck to it.
--
-- qr_token is what the QR encodes, NOT the row id. Sequential or guessable
-- identifiers would let anyone enumerate every table on the platform and, once
-- ordering ships in Phase 5, place orders against them. The token is random,
-- unique platform-wide, and can be rotated without touching the table row.
CREATE TABLE restaurant_tables (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
    label         text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 60),
    qr_token      text NOT NULL CHECK (length(qr_token) BETWEEN 8 AND 64),
    seats         integer CHECK (seats IS NULL OR seats BETWEEN 1 AND 100),
    sort_order    integer NOT NULL DEFAULT 0,
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Platform-wide: the token is the sole lookup key for a public request, so it
-- must not collide across restaurants.
CREATE UNIQUE INDEX restaurant_tables_token_uniq ON restaurant_tables (qr_token);

-- Two tables called "5" in one restaurant would make the printed QR sheets
-- ambiguous for the staff putting them out.
CREATE UNIQUE INDEX restaurant_tables_label_uniq
    ON restaurant_tables (restaurant_id, lower(btrim(label)));

CREATE INDEX restaurant_tables_restaurant_idx
    ON restaurant_tables (restaurant_id, sort_order);

CREATE TRIGGER restaurant_tables_set_updated_at
    BEFORE UPDATE ON restaurant_tables
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS restaurant_tables;
