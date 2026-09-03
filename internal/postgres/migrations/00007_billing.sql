-- +goose Up

-- Plans, subscriptions, invoices and payment transactions.
--
-- The shape here is driven by one rule from the architecture notes: a downgrade
-- or a missed payment must make an account *read-only*, never break the QR code
-- a guest is scanning right now. So nothing in this migration touches menus,
-- tables or orders — billing state is read on the write path only, and the
-- public surface never joins to it.

-- A plan is pricing configuration, not code. limits is jsonb so a new lever
-- (or a bespoke deal for one customer) is a row change rather than a deploy.
--
-- The primary key is the code, not a uuid: 'pro' is what appears in an invoice,
-- in a support conversation and in the provider's account reference, and a
-- surrogate key would only add a join to every one of those.
CREATE TABLE plans (
    code text PRIMARY KEY CHECK (code ~ '^[a-z][a-z0-9_]{0,30}$'),

    name        jsonb NOT NULL,
    description jsonb,

    price_minor bigint NOT NULL CHECK (price_minor >= 0),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    -- "interval" is a reserved word and a type name in Postgres, so the column
    -- carries the prefix rather than living in permanent double quotes.
    billing_interval text NOT NULL CHECK (billing_interval IN ('month', 'year')),
    trial_days       integer NOT NULL DEFAULT 0 CHECK (trial_days BETWEEN 0 AND 365),

    -- See domain.Limits for the keys. -1 means unlimited; a missing key reads
    -- as zero, so adding a lever defaults every existing plan to "not allowed"
    -- rather than silently granting it.
    limits jsonb NOT NULL DEFAULT '{}',

    -- Private plans are grandfathered pricing and custom deals: assignable, but
    -- never listed on the pricing page.
    is_public  boolean NOT NULL DEFAULT true,
    sort_order integer NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER plans_set_updated_at
    BEFORE UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Every tenant has exactly one live subscription, created at registration on
-- the free plan. Making "no subscription" impossible removes the branch that
-- would otherwise have to be written — and got wrong — at every limit check.
--
-- Cancelling does not delete the row; it moves the tenant back to free. The
-- partial unique index below is what holds the "exactly one" invariant.
CREATE TABLE subscriptions (
    id        uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,

    -- RESTRICT, not CASCADE: deleting a plan someone is paying for must fail
    -- loudly. Retiring a plan means making it private, not removing it.
    plan_code text NOT NULL REFERENCES plans(code) ON UPDATE CASCADE ON DELETE RESTRICT,

    status text NOT NULL DEFAULT 'active'
        CHECK (status IN ('trialing', 'active', 'past_due', 'suspended', 'cancelled')),

    provider     text NOT NULL DEFAULT 'manual'
        CHECK (provider IN ('manual', 'payme', 'click', 'stripe')),
    -- The provider's own handle for the recurring agreement, where it has one.
    provider_ref text,

    current_period_start timestamptz NOT NULL DEFAULT now(),
    -- NULL means "never expires", which is what a free plan is. The expiry
    -- worker only ever looks at rows where this is set.
    current_period_end   timestamptz,

    cancel_at_period_end boolean NOT NULL DEFAULT false,
    cancelled_at         timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT subscriptions_period_ordered
        CHECK (current_period_end IS NULL OR current_period_end > current_period_start)
);

CREATE UNIQUE INDEX subscriptions_tenant_live_uniq
    ON subscriptions (tenant_id) WHERE status <> 'cancelled';

-- Drives the daily expiry sweep. Partial, because the overwhelming majority of
-- rows are free plans with no period end and they should never be scanned.
CREATE INDEX subscriptions_expiry_idx
    ON subscriptions (current_period_end)
    WHERE current_period_end IS NOT NULL AND status IN ('trialing', 'active', 'past_due');

CREATE TRIGGER subscriptions_set_updated_at
    BEFORE UPDATE ON subscriptions FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- An invoice is the thing a payment is made against. It snapshots the plan
-- code, the amount and the currency for the same reason order_items snapshot a
-- dish name: what the customer was charged must survive a later price change.
CREATE TABLE invoices (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    subscription_id uuid NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,

    -- Human-readable and stable: this is the number quoted in an email or read
    -- out over the phone. Sequential per year, allocated as max+1 and retried
    -- on collision, exactly like the daily order number.
    number   text NOT NULL,
    year     integer NOT NULL,
    seq      integer NOT NULL CHECK (seq > 0),

    plan_code    text NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
    currency     text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    status text NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'paid', 'void', 'refunded')),

    period_start timestamptz NOT NULL,
    period_end   timestamptz NOT NULL,

    provider text NOT NULL
        CHECK (provider IN ('manual', 'payme', 'click', 'stripe')),

    paid_at timestamptz,
    -- Free text from an admin marking a manual payment, or the provider's
    -- reason for voiding. Never rendered as HTML.
    memo    text CHECK (memo IS NULL OR length(memo) <= 500),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT invoices_period_ordered CHECK (period_end > period_start),
    CONSTRAINT invoices_paid_has_time
        CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);

CREATE UNIQUE INDEX invoices_number_uniq ON invoices (number);
CREATE UNIQUE INDEX invoices_year_seq_uniq ON invoices (year, seq);
CREATE INDEX invoices_tenant_idx ON invoices (tenant_id, created_at DESC);
-- The one an unpaid-invoice sweep would use.
CREATE INDEX invoices_open_idx ON invoices (created_at) WHERE status = 'open';

CREATE TRIGGER invoices_set_updated_at
    BEFORE UPDATE ON invoices FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One row per attempt a provider makes to move money against an invoice.
--
-- This table *is* the webhook idempotency record. There is no separate
-- events table because the providers this targets are not event-push systems:
-- Payme calls a JSON-RPC endpoint whose whole contract is "look this
-- transaction up by id and tell me its state", and Click posts a prepare/
-- complete pair keyed the same way. The unique index below is what makes a
-- retried callback a lookup instead of a second charge.
CREATE TABLE payment_transactions (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    invoice_id uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    tenant_id  uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,

    provider text NOT NULL
        CHECK (provider IN ('manual', 'payme', 'click', 'stripe')),
    -- The provider's transaction id, verbatim. Text because none of them
    -- agree on a format and reformatting an identifier you must echo back is
    -- how you fail a reconciliation.
    provider_txn_id text NOT NULL,

    state text NOT NULL DEFAULT 'created'
        CHECK (state IN ('created', 'performed', 'cancelled')),

    amount_minor bigint NOT NULL CHECK (amount_minor >= 0),

    -- Providers timestamp in milliseconds since the epoch and expect the same
    -- value echoed back unchanged, so the raw number is stored alongside our
    -- own timestamptz rather than being converted and converted back.
    created_ms   bigint NOT NULL,
    performed_ms bigint,
    cancelled_ms bigint,
    -- Provider-specific numeric reason, kept as sent.
    cancel_reason integer,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payment_transactions_performed_has_time
        CHECK ((state = 'performed') = (performed_ms IS NOT NULL)),
    CONSTRAINT payment_transactions_cancelled_has_time
        CHECK ((state = 'cancelled') = (cancelled_ms IS NOT NULL))
);

CREATE UNIQUE INDEX payment_transactions_provider_txn_uniq
    ON payment_transactions (provider, provider_txn_id);
CREATE INDEX payment_transactions_invoice_idx
    ON payment_transactions (invoice_id);
-- Payme's GetStatement asks for every transaction in a time range.
CREATE INDEX payment_transactions_created_ms_idx
    ON payment_transactions (provider, created_ms);

-- At most one transaction may be in flight or settled per invoice. A provider
-- retrying CreateTransaction with a *different* id against an invoice that is
-- already being paid is a double charge, and this is where it stops.
CREATE UNIQUE INDEX payment_transactions_invoice_live_uniq
    ON payment_transactions (invoice_id) WHERE state <> 'cancelled';

CREATE TRIGGER payment_transactions_set_updated_at
    BEFORE UPDATE ON payment_transactions FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Seed plans. These are data, not schema, but they ship in the migration
-- because a running system with no 'free' plan has no valid state for a new
-- tenant — registration would fail on the foreign key.
--
-- Prices are UZS, which internal/money treats as zero-decimal: 149000 is
-- 149 000 so'm, not 1 490.00.
INSERT INTO plans (code, name, description, price_minor, currency, billing_interval, trial_days, limits, sort_order) VALUES
    ('free',
     '{"uz": "Bepul", "ru": "Бесплатный", "en": "Free"}',
     '{"uz": "Bitta restoran, bitta menyu.", "ru": "Один ресторан, одно меню.", "en": "One restaurant, one menu."}',
     0, 'UZS', 'month', 0,
     '{"restaurants": 1, "menu_items": 30, "tables": 5, "staff_users": 2, "locales": 1, "storage_mb": 50, "orders_enabled": false, "custom_brand": false, "remove_watermark": false}',
     10),
    ('basic',
     '{"uz": "Boshlang''ich", "ru": "Базовый", "en": "Basic"}',
     '{"uz": "Kichik kafe uchun.", "ru": "Для небольшого кафе.", "en": "For a single venue."}',
     149000, 'UZS', 'month', 14,
     '{"restaurants": 1, "menu_items": 200, "tables": 30, "staff_users": 10, "locales": 3, "storage_mb": 500, "orders_enabled": true, "custom_brand": true, "remove_watermark": false}',
     20),
    ('pro',
     '{"uz": "Pro", "ru": "Про", "en": "Pro"}',
     '{"uz": "Bir nechta filial uchun.", "ru": "Для сети заведений.", "en": "For a group of venues."}',
     399000, 'UZS', 'month', 14,
     '{"restaurants": -1, "menu_items": -1, "tables": -1, "staff_users": -1, "locales": -1, "storage_mb": 5000, "orders_enabled": true, "custom_brand": true, "remove_watermark": true}',
     30);

-- Existing tenants predate billing and must not be locked out by it: put every
-- one of them on free, which is also what registration will do from now on.
INSERT INTO subscriptions (tenant_id, plan_code, status, provider)
SELECT id, 'free', 'active', 'manual' FROM tenants;

-- +goose Down
DROP TABLE IF EXISTS payment_transactions;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plans;
