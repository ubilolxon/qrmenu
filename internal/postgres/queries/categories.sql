-- name: CreateCategory :one
INSERT INTO categories (restaurant_id, menu_id, name, description, sort_order)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetCategory :one
SELECT * FROM categories WHERE id = $1 AND restaurant_id = $2;

-- name: ListCategories :many
SELECT * FROM categories
WHERE menu_id = $1 AND restaurant_id = $2
ORDER BY sort_order, created_at;

-- name: ListCategoriesByRestaurant :many
SELECT * FROM categories
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- name: UpdateCategory :one
UPDATE categories
SET name        = COALESCE(sqlc.narg(name)::jsonb, name),
    description = COALESCE(sqlc.narg(description)::jsonb, description),
    is_active   = COALESCE(sqlc.narg(is_active)::boolean, is_active),
    sort_order  = COALESCE(sqlc.narg(sort_order)::integer, sort_order),
    -- Moving a category to another menu is allowed; the composite foreign key
    -- rejects a menu belonging to a different restaurant.
    menu_id     = COALESCE(sqlc.narg(menu_id)::uuid, menu_id)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- name: ReorderCategories :execrows
UPDATE categories c
SET sort_order = v.ord::integer
FROM unnest(sqlc.arg(ids)::uuid[]) WITH ORDINALITY AS v(id, ord)
WHERE c.id = v.id AND c.restaurant_id = sqlc.arg(restaurant_id);

-- name: DeleteCategory :execrows
DELETE FROM categories WHERE id = $1 AND restaurant_id = $2;
