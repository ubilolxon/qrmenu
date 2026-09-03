-- +goose Up

-- Row-level security as a **backstop**, not as the tenancy mechanism.
--
-- The isolation this service relies on is still the three layers above it:
-- every repository method takes the scope as its first argument, the scope
-- comes from the verified token and never from a request body, and every
-- predicate is visible in a hand-written .sql file. This migration adds a
-- fourth layer underneath them, so that a query which forgets its WHERE returns
-- zero rows instead of another tenant's data.
--
-- Two things about it are worth being precise about, because a backstop that is
-- believed to do more than it does is worse than none.
--
--   1. It only binds when app.tenant_id is set. It is set on every
--      authenticated request (see internal/postgres.Connect), and deliberately
--      NOT set on the paths that have no tenant: migrations, the login lookup
--      that resolves which tenant an address belongs to, and the whole public
--      menu surface, where the credential is an unguessable QR token rather
--      than an account. Those paths are exactly as protected as they were
--      before, which is to say by their own WHERE clauses.
--
--   2. FORCE is not optional. Policies do not apply to a table's owner, and the
--      application connects as the role that owns these tables. Without FORCE
--      every policy below would parse, apply to nobody, and produce a
--      convincing-looking pg_policies listing that protects nothing.

CREATE SCHEMA IF NOT EXISTS app;

-- +goose StatementBegin
CREATE FUNCTION app.current_tenant() RETURNS uuid
LANGUAGE plpgsql STABLE AS $$
DECLARE
    raw text := current_setting('app.tenant_id', true);
BEGIN
    IF raw IS NULL OR raw = '' THEN
        RETURN NULL;
    END IF;
    RETURN raw::uuid;
EXCEPTION WHEN invalid_text_representation THEN
    -- A malformed setting must not read as "no tenant", which would open every
    -- row. Refuse instead: a nil uuid matches nothing.
    RETURN '00000000-0000-0000-0000-000000000000'::uuid;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION app.current_tenant() IS
    'The tenant of the current connection, or NULL when unscoped. STABLE so a policy evaluates it once per statement rather than once per row.';

-- +goose StatementBegin
-- Applying the same policy shape to nineteen tables by hand is nineteen chances
-- to get one wrong, and the one that is wrong is the one nobody notices. The
-- table lists below are explicit — a new table is not silently covered — but
-- the policy text is generated, so every table in a list gets exactly the same
-- rule.
DO $$
DECLARE
    t text;
BEGIN
    -- Tables that carry the tenant directly.
    FOREACH t IN ARRAY ARRAY['restaurants', 'users', 'subscriptions', 'invoices', 'payment_transactions']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($f$
            CREATE POLICY tenant_isolation ON %I
            USING (app.current_tenant() IS NULL OR tenant_id = app.current_tenant())
            WITH CHECK (app.current_tenant() IS NULL OR tenant_id = app.current_tenant())
        $f$, t);
    END LOOP;

    -- Tables scoped to a restaurant. Every one of them carries restaurant_id
    -- outright — the composite-foreign-key design from Phase 2 means no policy
    -- here has to walk a chain of parents to find its tenant.
    FOREACH t IN ARRAY ARRAY[
        'restaurant_tables', 'menus', 'categories', 'items', 'item_variants',
        'modifier_groups', 'modifiers', 'images', 'orders', 'order_items', 'waiter_calls'
    ]
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        -- The subquery is evaluated once per statement, not once per row:
        -- app.current_tenant() is STABLE, so the planner hoists this into a
        -- hashed InitPlan. A restaurant list is a handful of rows.
        EXECUTE format($f$
            CREATE POLICY tenant_isolation ON %I
            USING (
                app.current_tenant() IS NULL
                OR restaurant_id IN (SELECT r.id FROM restaurants r WHERE r.tenant_id = app.current_tenant())
            )
            WITH CHECK (
                app.current_tenant() IS NULL
                OR restaurant_id IN (SELECT r.id FROM restaurants r WHERE r.tenant_id = app.current_tenant())
            )
        $f$, t);
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- The tenant row itself is identified by its primary key rather than a column
-- named tenant_id.
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenants
    USING (app.current_tenant() IS NULL OR id = app.current_tenant())
    WITH CHECK (app.current_tenant() IS NULL OR id = app.current_tenant());

-- Refresh tokens reach their tenant through the user they belong to. This is
-- the one table where that indirection is unavoidable, and it is also the one
-- table whose hot path — token refresh — runs unscoped anyway, because the
-- whole point of the request is to work out who the caller is.
ALTER TABLE refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON refresh_tokens
    USING (
        app.current_tenant() IS NULL
        OR user_id IN (SELECT u.id FROM users u WHERE u.tenant_id = app.current_tenant())
    )
    WITH CHECK (
        app.current_tenant() IS NULL
        OR user_id IN (SELECT u.id FROM users u WHERE u.tenant_id = app.current_tenant())
    );

-- plans is deliberately left open: it is the price list, the same for everyone,
-- and read by the unauthenticated pricing page.

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'restaurants', 'users', 'subscriptions', 'invoices', 'payment_transactions',
        'restaurant_tables', 'menus', 'categories', 'items', 'item_variants',
        'modifier_groups', 'modifiers', 'images', 'orders', 'order_items',
        'waiter_calls', 'tenants', 'refresh_tokens'
    ]
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
    END LOOP;
END;
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS app.current_tenant();
DROP SCHEMA IF EXISTS app;
