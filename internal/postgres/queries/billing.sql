-- name: ListPublicPlans :many
SELECT * FROM plans WHERE is_public ORDER BY sort_order, price_minor;

-- name: ListPlans :many
SELECT * FROM plans ORDER BY sort_order, price_minor;

-- name: GetPlan :one
SELECT * FROM plans WHERE code = $1;

-- name: CreateSubscription :one
INSERT INTO subscriptions (tenant_id, plan_code, status, provider, current_period_end)
VALUES ($1, $2, $3, $4, sqlc.narg(current_period_end)::timestamptz)
RETURNING *;

-- A tenant has exactly one non-cancelled subscription, held by
-- subscriptions_tenant_live_uniq, so this can never return two rows.
--
-- name: GetSubscription :one
SELECT * FROM subscriptions
WHERE tenant_id = $1 AND status <> 'cancelled';

-- GetEntitlement is the query on the write path of every limit check, so it is
-- one indexed lookup and one primary-key join rather than two round trips.
--
-- name: GetEntitlement :one
SELECT
    sqlc.embed(s),
    p.limits          AS plan_limits,
    p.name            AS plan_name,
    p.price_minor     AS plan_price_minor,
    p.currency        AS plan_currency,
    p.billing_interval AS plan_billing_interval
FROM subscriptions s
JOIN plans p ON p.code = s.plan_code
WHERE s.tenant_id = $1 AND s.status <> 'cancelled';

-- Activate is the single path a paid subscription takes, whether the payment
-- was a renewal, an upgrade or a downgrade.
--
-- The new period starts at the old one's end when the plan is unchanged, so a
-- payment that lands two days late does not shorten the month the customer
-- bought. Any *other* plan starts now: someone upgrading mid-month is paying to
-- get the bigger plan today, not at the end of the one they are leaving.
--
-- name: ActivateSubscription :one
WITH base AS (
    SELECT s.id,
           CASE WHEN s.plan_code = sqlc.arg(plan_code) AND s.current_period_end IS NOT NULL
                THEN greatest(s.current_period_end, now())
                ELSE now()
           END AS starts_at
    FROM subscriptions s
    WHERE s.id = sqlc.arg(id)
)
UPDATE subscriptions s
SET plan_code            = sqlc.arg(plan_code),
    status               = 'active',
    provider             = sqlc.arg(provider)::text,
    provider_ref         = COALESCE(sqlc.narg(provider_ref)::text, s.provider_ref),
    current_period_start = base.starts_at,
    current_period_end   = base.starts_at + make_interval(months => sqlc.arg(months)::int),
    cancel_at_period_end = false
FROM base
WHERE s.id = base.id
RETURNING s.*;

-- Moving to the free plan is its own statement rather than a zero-month
-- activation: a free plan has no period at all, and expressing "never expires"
-- as an interval of zero would make it expire immediately.
--
-- name: SetFreePlan :one
UPDATE subscriptions
SET plan_code            = 'free',
    status               = 'active',
    provider             = 'manual',
    provider_ref         = NULL,
    current_period_start = now(),
    current_period_end   = NULL,
    cancel_at_period_end = false
WHERE id = $1
RETURNING *;

-- name: SetSubscriptionStatus :one
UPDATE subscriptions
SET status       = sqlc.arg(status)::text,
    cancelled_at = CASE WHEN sqlc.arg(status)::text = 'cancelled' AND cancelled_at IS NULL
                        THEN now() ELSE cancelled_at END
WHERE id = sqlc.arg(id)
  AND status = sqlc.arg(from_status)::text
RETURNING *;

-- name: SetSubscriptionCancelAtPeriodEnd :one
UPDATE subscriptions
SET cancel_at_period_end = sqlc.arg(cancel_at_period_end)
WHERE tenant_id = sqlc.arg(tenant_id) AND status <> 'cancelled'
RETURNING *;

-- The first half of the daily sweep: a period that has run out stops being
-- active. A subscription the customer already asked to end goes straight to
-- cancelled and back to free; everything else gets the grace period.
--
-- name: ExpireSubscriptions :many
UPDATE subscriptions
SET status       = CASE WHEN cancel_at_period_end THEN 'cancelled' ELSE 'past_due' END,
    plan_code    = CASE WHEN cancel_at_period_end THEN 'free' ELSE plan_code END,
    cancelled_at = CASE WHEN cancel_at_period_end THEN now() ELSE cancelled_at END
WHERE status IN ('trialing', 'active')
  AND current_period_end IS NOT NULL
  AND current_period_end < now()
RETURNING *;

-- The second half: an unpaid subscription that has also used up its grace is
-- suspended. Suspension freezes the tenant's own writes; it does not touch the
-- public menu or a guest's order in progress.
--
-- name: SuspendPastDueSubscriptions :many
UPDATE subscriptions
SET status = 'suspended'
WHERE status = 'past_due'
  AND current_period_end IS NOT NULL
  AND current_period_end < now() - make_interval(secs => sqlc.arg(grace_seconds)::double precision)
RETURNING *;

-- name: NextInvoiceNumber :one
SELECT
    extract(year FROM now())::integer                 AS year,
    (coalesce(max(i.seq), 0) + 1)::integer            AS next_seq
FROM invoices i
WHERE i.year = extract(year FROM now())::integer;

-- name: CreateInvoice :one
INSERT INTO invoices (
    tenant_id, subscription_id, number, year, seq, plan_code,
    amount_minor, currency, status, period_start, period_end, provider, memo
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, sqlc.narg(memo)::text
)
RETURNING *;

-- GetInvoiceByID is deliberately not tenant-scoped: a payment provider knows
-- only the invoice id it was handed at checkout, and has no tenant context to
-- offer. Never call it from a request handler — use GetInvoice, which takes
-- the tenant.
--
-- name: GetInvoiceByID :one
SELECT * FROM invoices WHERE id = $1;

-- name: GetInvoice :one
SELECT * FROM invoices WHERE id = $1 AND tenant_id = $2;

-- name: ListInvoices :many
SELECT * FROM invoices
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- Compare-and-set from 'open': a provider that delivers the same success
-- callback twice marks the invoice paid once, and the second attempt returns
-- no rows rather than re-running the renewal behind it.
--
-- name: MarkInvoicePaid :one
UPDATE invoices
SET status  = 'paid',
    paid_at = now(),
    memo    = COALESCE(sqlc.narg(memo)::text, memo)
WHERE id = sqlc.arg(id) AND status = 'open'
RETURNING *;

-- name: SetInvoiceStatus :one
UPDATE invoices
SET status  = sqlc.arg(status)::text,
    paid_at = CASE WHEN sqlc.arg(status)::text = 'paid' THEN coalesce(paid_at, now()) ELSE NULL END,
    memo    = COALESCE(sqlc.narg(memo)::text, memo)
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status)::text
RETURNING *;

-- name: CreatePaymentTransaction :one
INSERT INTO payment_transactions (
    invoice_id, tenant_id, provider, provider_txn_id, amount_minor, created_ms
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetPaymentTransaction :one
SELECT * FROM payment_transactions
WHERE provider = $1 AND provider_txn_id = $2;

-- name: GetLivePaymentTransaction :one
SELECT * FROM payment_transactions
WHERE invoice_id = $1 AND state <> 'cancelled';

-- name: PerformPaymentTransaction :one
UPDATE payment_transactions
SET state = 'performed', performed_ms = sqlc.arg(performed_ms)
WHERE id = sqlc.arg(id) AND state = 'created'
RETURNING *;

-- Cancellable from either state: a provider may reverse a payment it has
-- already performed, which is a refund rather than an abandoned checkout, and
-- the reason code is what tells the two apart.
--
-- name: CancelPaymentTransaction :one
UPDATE payment_transactions
SET state         = 'cancelled',
    cancelled_ms  = sqlc.arg(cancelled_ms),
    cancel_reason = sqlc.narg(cancel_reason)::integer
WHERE id = sqlc.arg(id) AND state <> 'cancelled'
RETURNING *;

-- name: ListPaymentTransactionsByRange :many
SELECT * FROM payment_transactions
WHERE provider = $1 AND created_ms BETWEEN $2 AND $3
ORDER BY created_ms;

-- Usage counters the limit checks compare against. Each one counts what the
-- corresponding plan lever caps, across the whole tenant rather than one
-- restaurant, because that is the unit a plan is sold in.

-- name: CountTablesByTenant :one
SELECT count(*) FROM restaurant_tables t
JOIN restaurants r ON r.id = t.restaurant_id
WHERE r.tenant_id = $1;

-- name: CountActiveUsersByTenant :one
SELECT count(*) FROM users WHERE tenant_id = $1 AND is_active;

-- Storage counts only images that made it to 'ready': a pending row has no
-- bytes in the bucket yet, and a failed one had its object removed.
--
-- name: SumImageBytesByTenant :one
SELECT coalesce(sum(i.byte_size), 0)::bigint FROM images i
JOIN restaurants r ON r.id = i.restaurant_id
WHERE r.tenant_id = $1 AND i.status = 'ready';
