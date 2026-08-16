-- +goose Up

-- A tenant is the paying account. A restaurant is one of its branches.
-- Billing counts across a tenant; menus, tables and orders belong to a
-- restaurant. Keeping them separate from the start is what makes restaurant
-- chains possible without a later rewrite.
CREATE TABLE tenants (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    name       text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    status     text NOT NULL DEFAULT 'active'
               CHECK (status IN ('active', 'suspended', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER tenants_set_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Email is unique globally, not per tenant, so login needs only an address and
-- a password. Scoping it per tenant would force the user to name their tenant
-- before authenticating, which is a worse first experience than the rare case
-- of one person wanting two accounts on the same address.
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email         citext NOT NULL,
    password_hash text NOT NULL,
    name          text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    role          text NOT NULL CHECK (role IN ('owner', 'manager', 'waiter', 'kitchen')),
    is_active     boolean NOT NULL DEFAULT true,
    last_login_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_email_uniq ON users (email);
CREATE INDEX users_tenant_id_idx ON users (tenant_id);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Refresh tokens are opaque random strings; only their SHA-256 is stored, so a
-- database leak does not hand over live sessions.
--
-- Rotation: using a token marks it used and issues a replacement in the same
-- family. Presenting an already-used token means it leaked, so the whole family
-- is revoked rather than just that one token.
CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id  uuid NOT NULL,
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    revoked_at timestamptz,
    user_agent text,
    ip         text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX refresh_tokens_hash_uniq ON refresh_tokens (token_hash);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
-- Supports the worker that prunes expired rows.
CREATE INDEX refresh_tokens_expires_idx ON refresh_tokens (expires_at);

-- Translatable fields are jsonb: {"uz": "Osh", "ru": "Плов"}. Adding a language
-- is a data change, not a migration. See internal/i18n for the fallback rules.
CREATE TABLE restaurants (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id      uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    slug           citext NOT NULL,
    name           jsonb NOT NULL,
    description    jsonb NOT NULL DEFAULT '{}'::jsonb,
    currency       text NOT NULL DEFAULT 'UZS' CHECK (length(currency) = 3),
    timezone       text NOT NULL DEFAULT 'Asia/Tashkent',
    default_locale text NOT NULL DEFAULT 'uz',
    locales        text[] NOT NULL DEFAULT ARRAY['uz'],
    phone          text,
    address        text,
    theme          jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Bumped on any menu write; forms the public cache key so invalidation is
    -- a counter increment rather than key-hunting. See Phase 3.
    menu_version   integer NOT NULL DEFAULT 1,
    is_active      boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- The default locale must be one the restaurant actually publishes,
    -- otherwise every fallback lookup dead-ends.
    CONSTRAINT restaurants_default_locale_listed
        CHECK (default_locale = ANY (locales)),
    CONSTRAINT restaurants_locales_not_empty
        CHECK (cardinality(locales) > 0)
);

-- The slug is the public menu URL, so it is unique across the whole platform,
-- not just within a tenant.
CREATE UNIQUE INDEX restaurants_slug_uniq ON restaurants (slug);
CREATE INDEX restaurants_tenant_idx ON restaurants (tenant_id);

CREATE TRIGGER restaurants_set_updated_at
    BEFORE UPDATE ON restaurants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS restaurants;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
