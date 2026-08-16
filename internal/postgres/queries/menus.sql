-- Every query in this file is scoped by restaurant_id. The service verifies the
-- restaurant belongs to the caller's tenant before any of these run, so the two
-- checks together are what keep one restaurant's menu out of another's reach.

-- name: CreateMenu :one
INSERT INTO menus (
    restaurant_id, name, description, sort_order,
    available_from_min, available_to_min, days
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetMenu :one
SELECT * FROM menus WHERE id = $1 AND restaurant_id = $2;

-- name: ListMenus :many
SELECT * FROM menus
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- name: UpdateMenu :one
UPDATE menus
SET name               = COALESCE(sqlc.narg(name)::jsonb, name),
    description        = COALESCE(sqlc.narg(description)::jsonb, description),
    is_active          = COALESCE(sqlc.narg(is_active)::boolean, is_active),
    sort_order         = COALESCE(sqlc.narg(sort_order)::integer, sort_order),
    days               = COALESCE(sqlc.narg(days)::smallint[], days),
    -- The window is set as a pair, so clear_window is how a caller removes it;
    -- COALESCE alone could never distinguish "unset me" from "leave alone".
    available_from_min = CASE WHEN sqlc.arg(clear_window)::boolean THEN NULL
                             ELSE COALESCE(sqlc.narg(available_from_min)::integer, available_from_min) END,
    available_to_min   = CASE WHEN sqlc.arg(clear_window)::boolean THEN NULL
                             ELSE COALESCE(sqlc.narg(available_to_min)::integer, available_to_min) END
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- Bulk reorder in one round trip. The array's order IS the new order --
-- WITH ORDINALITY numbers the elements, so the client sends ids in the desired
-- sequence and nothing has to stay in sync with a parallel positions array.
--
-- The restaurant_id predicate still applies to every row, so a foreign id
-- smuggled into the list simply matches nothing rather than reordering someone
-- else's menu.
--
-- name: ReorderMenus :execrows
UPDATE menus m
SET sort_order = v.ord::integer
FROM unnest(sqlc.arg(ids)::uuid[]) WITH ORDINALITY AS v(id, ord)
WHERE m.id = v.id AND m.restaurant_id = sqlc.arg(restaurant_id);

-- name: DeleteMenu :execrows
DELETE FROM menus WHERE id = $1 AND restaurant_id = $2;
