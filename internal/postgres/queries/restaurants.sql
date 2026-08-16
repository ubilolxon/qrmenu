-- name: CreateRestaurant :one
INSERT INTO restaurants (
    tenant_id, slug, name, description, currency, timezone,
    default_locale, locales, phone, address
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetRestaurant :one
SELECT * FROM restaurants
WHERE id = $1 AND tenant_id = $2;

-- GetRestaurantBySlug serves the public menu, which has no tenant context —
-- the slug is the guest's only identifier. Unscoped by design; it returns
-- nothing for an inactive restaurant so a suspended account goes dark.
--
-- name: GetRestaurantBySlug :one
SELECT * FROM restaurants
WHERE slug = $1 AND is_active;

-- name: ListRestaurants :many
SELECT * FROM restaurants
WHERE tenant_id = $1
ORDER BY created_at;

-- name: CountRestaurants :one
SELECT count(*) FROM restaurants WHERE tenant_id = $1;

-- Partial update: every field is nullable, and a NULL argument means "leave
-- this alone". The casts are required for sqlc to infer a type for an argument
-- that only ever appears inside COALESCE.
--
-- name: UpdateRestaurant :one
UPDATE restaurants
SET slug           = COALESCE(sqlc.narg(slug)::citext, slug),
    name           = COALESCE(sqlc.narg(name)::jsonb, name),
    description    = COALESCE(sqlc.narg(description)::jsonb, description),
    currency       = COALESCE(sqlc.narg(currency)::text, currency),
    timezone       = COALESCE(sqlc.narg(timezone)::text, timezone),
    default_locale = COALESCE(sqlc.narg(default_locale)::text, default_locale),
    locales        = COALESCE(sqlc.narg(locales)::text[], locales),
    phone          = COALESCE(sqlc.narg(phone)::text, phone),
    address        = COALESCE(sqlc.narg(address)::text, address),
    theme          = COALESCE(sqlc.narg(theme)::jsonb, theme),
    is_active      = COALESCE(sqlc.narg(is_active)::boolean, is_active)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- Bumping the version is how the public menu cache is invalidated: the counter
-- is part of the cache key, so old entries are simply never read again and
-- expire on their own. See Phase 3.
--
-- name: BumpMenuVersion :one
UPDATE restaurants
SET menu_version = menu_version + 1
WHERE id = $1 AND tenant_id = $2
RETURNING menu_version;

-- name: DeleteRestaurant :execrows
DELETE FROM restaurants WHERE id = $1 AND tenant_id = $2;
