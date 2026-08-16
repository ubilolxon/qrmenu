-- +goose Up

-- The menu tree: menus → categories → items → (variants, modifier groups →
-- modifiers).
--
-- Every table carries restaurant_id, even though it could be reached by
-- following parent links. Two reasons:
--
--  1. Scoping. A query on modifiers can be scoped with one predicate instead of
--     a four-table join. The rule "every query filters by the scope" only stays
--     practical if the scope is one column away.
--  2. Caching and bulk reads. Phase 3 loads an entire menu for one restaurant;
--     that is a handful of indexed scans rather than a join pyramid.
--
-- The usual objection to denormalising is that it can drift out of sync. That
-- cannot happen here: each child references its parent with a COMPOSITE foreign
-- key on (parent_id, restaurant_id), so Postgres itself refuses to let a
-- category belong to a menu from a different restaurant. The redundancy is
-- enforced, not merely intended.

CREATE TABLE menus (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
    name          jsonb NOT NULL,
    description   jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_active     boolean NOT NULL DEFAULT true,
    sort_order    integer NOT NULL DEFAULT 0,

    -- Availability window as minutes from midnight in the restaurant's local
    -- timezone (0..1439; 13:30 is 810). Plain integers rather than `time`
    -- because they compare, serialise to JSON, and cross language boundaries
    -- without a dedicated type on either side.
    --
    -- Both NULL means always available. A window that wraps midnight
    -- (from 1320 to 120) is legal and handled where it is evaluated.
    available_from_min integer CHECK (available_from_min BETWEEN 0 AND 1439),
    available_to_min   integer CHECK (available_to_min BETWEEN 0 AND 1439),
    -- ISO day numbers, Monday = 1 .. Sunday = 7, matching Postgres isodow.
    -- Empty means every day.
    days           smallint[] NOT NULL DEFAULT '{}',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT menus_days_valid CHECK (days <@ ARRAY[1,2,3,4,5,6,7]::smallint[]),
    CONSTRAINT menus_window_paired CHECK (
        (available_from_min IS NULL) = (available_to_min IS NULL)
    ),
    -- Required so children can reference (id, restaurant_id) compositely.
    CONSTRAINT menus_id_restaurant_uniq UNIQUE (id, restaurant_id)
);

CREATE INDEX menus_restaurant_idx ON menus (restaurant_id, sort_order);

CREATE TRIGGER menus_set_updated_at
    BEFORE UPDATE ON menus FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE categories (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL,
    menu_id       uuid NOT NULL,
    name          jsonb NOT NULL,
    description   jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_active     boolean NOT NULL DEFAULT true,
    sort_order    integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- The composite reference is what makes restaurant_id trustworthy: a
    -- category cannot point at a menu owned by a different restaurant.
    CONSTRAINT categories_menu_fk FOREIGN KEY (menu_id, restaurant_id)
        REFERENCES menus (id, restaurant_id) ON DELETE CASCADE,
    CONSTRAINT categories_id_restaurant_uniq UNIQUE (id, restaurant_id)
);

CREATE INDEX categories_menu_idx ON categories (menu_id, sort_order);
CREATE INDEX categories_restaurant_idx ON categories (restaurant_id);

CREATE TRIGGER categories_set_updated_at
    BEFORE UPDATE ON categories FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE items (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL,
    category_id   uuid NOT NULL,
    name          jsonb NOT NULL,
    description   jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Money is an integer count of minor units (tiyin, cents). The currency
    -- lives on the restaurant. Never a float: 0.1 + 0.2 is not 0.3, and a menu
    -- that cannot add up is worse than no menu.
    price_minor   bigint NOT NULL CHECK (price_minor >= 0),

    is_available  boolean NOT NULL DEFAULT true,
    sort_order    integer NOT NULL DEFAULT 0,
    tags          text[] NOT NULL DEFAULT '{}',
    allergens     text[] NOT NULL DEFAULT '{}',
    calories      integer CHECK (calories IS NULL OR calories >= 0),
    prep_minutes  integer CHECK (prep_minutes IS NULL OR prep_minutes >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT items_category_fk FOREIGN KEY (category_id, restaurant_id)
        REFERENCES categories (id, restaurant_id) ON DELETE CASCADE,
    CONSTRAINT items_id_restaurant_uniq UNIQUE (id, restaurant_id)
);

CREATE INDEX items_category_idx ON items (category_id, sort_order);
CREATE INDEX items_restaurant_idx ON items (restaurant_id);

CREATE TRIGGER items_set_updated_at
    BEFORE UPDATE ON items FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Sizes: Small / Medium / Large. The delta is added to the item price, so
-- raising the base price does not require touching every variant.
CREATE TABLE item_variants (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id     uuid NOT NULL,
    item_id           uuid NOT NULL,
    name              jsonb NOT NULL,
    price_delta_minor bigint NOT NULL DEFAULT 0,
    is_default        boolean NOT NULL DEFAULT false,
    is_available      boolean NOT NULL DEFAULT true,
    sort_order        integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT item_variants_item_fk FOREIGN KEY (item_id, restaurant_id)
        REFERENCES items (id, restaurant_id) ON DELETE CASCADE
);

CREATE INDEX item_variants_item_idx ON item_variants (item_id, sort_order);
CREATE INDEX item_variants_restaurant_idx ON item_variants (restaurant_id);

-- At most one default variant per item, enforced by the database rather than
-- by hoping the application checks.
CREATE UNIQUE INDEX item_variants_one_default
    ON item_variants (item_id) WHERE is_default;

CREATE TRIGGER item_variants_set_updated_at
    BEFORE UPDATE ON item_variants FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- "Choose your sauce (pick 1)", "Extras (up to 3)".
CREATE TABLE modifier_groups (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL,
    item_id       uuid NOT NULL,
    name          jsonb NOT NULL,
    min_select    integer NOT NULL DEFAULT 0 CHECK (min_select >= 0),
    max_select    integer NOT NULL DEFAULT 1 CHECK (max_select >= 1),
    sort_order    integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT modifier_groups_range CHECK (min_select <= max_select),
    CONSTRAINT modifier_groups_item_fk FOREIGN KEY (item_id, restaurant_id)
        REFERENCES items (id, restaurant_id) ON DELETE CASCADE,
    CONSTRAINT modifier_groups_id_restaurant_uniq UNIQUE (id, restaurant_id)
);

CREATE INDEX modifier_groups_item_idx ON modifier_groups (item_id, sort_order);
CREATE INDEX modifier_groups_restaurant_idx ON modifier_groups (restaurant_id);

CREATE TRIGGER modifier_groups_set_updated_at
    BEFORE UPDATE ON modifier_groups FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE modifiers (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id     uuid NOT NULL,
    group_id          uuid NOT NULL,
    name              jsonb NOT NULL,
    price_delta_minor bigint NOT NULL DEFAULT 0,
    is_available      boolean NOT NULL DEFAULT true,
    sort_order        integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT modifiers_group_fk FOREIGN KEY (group_id, restaurant_id)
        REFERENCES modifier_groups (id, restaurant_id) ON DELETE CASCADE
);

CREATE INDEX modifiers_group_idx ON modifiers (group_id, sort_order);
CREATE INDEX modifiers_restaurant_idx ON modifiers (restaurant_id);

CREATE TRIGGER modifiers_set_updated_at
    BEFORE UPDATE ON modifiers FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS modifiers;
DROP TABLE IF EXISTS modifier_groups;
DROP TABLE IF EXISTS item_variants;
DROP TABLE IF EXISTS items;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS menus;
