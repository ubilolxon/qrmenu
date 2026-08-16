-- name: CreateItem :one
INSERT INTO items (
    restaurant_id, category_id, name, description, price_minor,
    sort_order, tags, allergens, calories, prep_minutes
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetItem :one
SELECT * FROM items WHERE id = $1 AND restaurant_id = $2;

-- name: ListItems :many
SELECT * FROM items
WHERE category_id = $1 AND restaurant_id = $2
ORDER BY sort_order, created_at;

-- Loads every item for a restaurant in one scan. Phase 3 builds the whole
-- public menu payload from queries like this rather than per-category round
-- trips.
--
-- name: ListItemsByRestaurant :many
SELECT * FROM items
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- name: UpdateItem :one
UPDATE items
SET name         = COALESCE(sqlc.narg(name)::jsonb, name),
    description  = COALESCE(sqlc.narg(description)::jsonb, description),
    price_minor  = COALESCE(sqlc.narg(price_minor)::bigint, price_minor),
    is_available = COALESCE(sqlc.narg(is_available)::boolean, is_available),
    sort_order   = COALESCE(sqlc.narg(sort_order)::integer, sort_order),
    tags         = COALESCE(sqlc.narg(tags)::text[], tags),
    allergens    = COALESCE(sqlc.narg(allergens)::text[], allergens),
    calories     = CASE WHEN sqlc.arg(clear_calories)::boolean THEN NULL
                        ELSE COALESCE(sqlc.narg(calories)::integer, calories) END,
    prep_minutes = CASE WHEN sqlc.arg(clear_prep_minutes)::boolean THEN NULL
                        ELSE COALESCE(sqlc.narg(prep_minutes)::integer, prep_minutes) END,
    category_id  = COALESCE(sqlc.narg(category_id)::uuid, category_id)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- Flipping availability is the single most frequent menu write — a dish runs
-- out mid-service — so it gets its own narrow query rather than a full update.
--
-- name: SetItemAvailability :one
UPDATE items
SET is_available = sqlc.arg(is_available)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- name: ReorderItems :execrows
UPDATE items i
SET sort_order = v.ord::integer
FROM unnest(sqlc.arg(ids)::uuid[]) WITH ORDINALITY AS v(id, ord)
WHERE i.id = v.id AND i.restaurant_id = sqlc.arg(restaurant_id);

-- name: CountItemsByRestaurant :one
SELECT count(*) FROM items WHERE restaurant_id = $1;

-- Counts every item a tenant owns across all their restaurants. This is the
-- number a plan limit is checked against in Phase 6.
--
-- name: CountItemsByTenant :one
SELECT count(*) FROM items i
JOIN restaurants r ON r.id = i.restaurant_id
WHERE r.tenant_id = $1;

-- name: DeleteItem :execrows
DELETE FROM items WHERE id = $1 AND restaurant_id = $2;
