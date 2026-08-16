-- Variants (sizes) and modifier groups/modifiers (extras) both hang off an
-- item and follow the same scoping rule as everything else.

-- name: CreateVariant :one
INSERT INTO item_variants (
    restaurant_id, item_id, name, price_delta_minor, is_default, sort_order
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetVariant :one
SELECT * FROM item_variants WHERE id = $1 AND restaurant_id = $2;

-- name: ListVariants :many
SELECT * FROM item_variants
WHERE item_id = $1 AND restaurant_id = $2
ORDER BY sort_order, created_at;

-- name: ListVariantsByRestaurant :many
SELECT * FROM item_variants
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- Clears the current default so a new one can be set. The partial unique index
-- allows only one default per item, so this must run first, in the same
-- transaction.
--
-- name: ClearDefaultVariant :execrows
UPDATE item_variants
SET is_default = false
WHERE item_id = $1 AND restaurant_id = $2 AND is_default;

-- name: UpdateVariant :one
UPDATE item_variants
SET name              = COALESCE(sqlc.narg(name)::jsonb, name),
    price_delta_minor = COALESCE(sqlc.narg(price_delta_minor)::bigint, price_delta_minor),
    is_default        = COALESCE(sqlc.narg(is_default)::boolean, is_default),
    is_available      = COALESCE(sqlc.narg(is_available)::boolean, is_available),
    sort_order        = COALESCE(sqlc.narg(sort_order)::integer, sort_order)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- name: DeleteVariant :execrows
DELETE FROM item_variants WHERE id = $1 AND restaurant_id = $2;

-- name: CreateModifierGroup :one
INSERT INTO modifier_groups (restaurant_id, item_id, name, min_select, max_select, sort_order)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetModifierGroup :one
SELECT * FROM modifier_groups WHERE id = $1 AND restaurant_id = $2;

-- name: ListModifierGroups :many
SELECT * FROM modifier_groups
WHERE item_id = $1 AND restaurant_id = $2
ORDER BY sort_order, created_at;

-- name: ListModifierGroupsByRestaurant :many
SELECT * FROM modifier_groups
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- name: UpdateModifierGroup :one
UPDATE modifier_groups
SET name       = COALESCE(sqlc.narg(name)::jsonb, name),
    min_select = COALESCE(sqlc.narg(min_select)::integer, min_select),
    max_select = COALESCE(sqlc.narg(max_select)::integer, max_select),
    sort_order = COALESCE(sqlc.narg(sort_order)::integer, sort_order)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- name: DeleteModifierGroup :execrows
DELETE FROM modifier_groups WHERE id = $1 AND restaurant_id = $2;

-- name: CreateModifier :one
INSERT INTO modifiers (restaurant_id, group_id, name, price_delta_minor, sort_order)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetModifier :one
SELECT * FROM modifiers WHERE id = $1 AND restaurant_id = $2;

-- name: ListModifiers :many
SELECT * FROM modifiers
WHERE group_id = $1 AND restaurant_id = $2
ORDER BY sort_order, created_at;

-- name: ListModifiersByRestaurant :many
SELECT * FROM modifiers
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- name: UpdateModifier :one
UPDATE modifiers
SET name              = COALESCE(sqlc.narg(name)::jsonb, name),
    price_delta_minor = COALESCE(sqlc.narg(price_delta_minor)::bigint, price_delta_minor),
    is_available      = COALESCE(sqlc.narg(is_available)::boolean, is_available),
    sort_order        = COALESCE(sqlc.narg(sort_order)::integer, sort_order)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- name: DeleteModifier :execrows
DELETE FROM modifiers WHERE id = $1 AND restaurant_id = $2;

-- Counts modifiers in a group, so the service can reject min_select greater
-- than the number of choices that actually exist.
--
-- name: CountModifiersInGroup :one
SELECT count(*) FROM modifiers WHERE group_id = $1 AND restaurant_id = $2;
