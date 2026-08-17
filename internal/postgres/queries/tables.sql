-- name: CreateTable :one
INSERT INTO restaurant_tables (restaurant_id, label, qr_token, seats, sort_order)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetTable :one
SELECT * FROM restaurant_tables WHERE id = $1 AND restaurant_id = $2;

-- GetTableByToken serves the public request. Unscoped by design: the token is
-- the guest's only identifier, and it is unguessable. The join brings the
-- restaurant along because the caller needs both, and it drops the row when the
-- restaurant is inactive so a suspended account goes dark.
--
-- name: GetTableByToken :one
SELECT sqlc.embed(t), sqlc.embed(r)
FROM restaurant_tables t
JOIN restaurants r ON r.id = t.restaurant_id
WHERE t.qr_token = $1 AND t.is_active AND r.is_active;

-- name: ListTables :many
SELECT * FROM restaurant_tables
WHERE restaurant_id = $1
ORDER BY sort_order, created_at;

-- name: UpdateTable :one
UPDATE restaurant_tables
SET label      = COALESCE(sqlc.narg(label)::text, label),
    seats      = CASE WHEN sqlc.arg(clear_seats)::boolean THEN NULL
                      ELSE COALESCE(sqlc.narg(seats)::integer, seats) END,
    sort_order = COALESCE(sqlc.narg(sort_order)::integer, sort_order),
    is_active  = COALESCE(sqlc.narg(is_active)::boolean, is_active)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- Rotating the token invalidates every printed QR for that table. Used when a
-- code leaks or a table is reassigned.
--
-- name: RotateTableToken :one
UPDATE restaurant_tables
SET qr_token = sqlc.arg(qr_token)
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- name: DeleteTable :execrows
DELETE FROM restaurant_tables WHERE id = $1 AND restaurant_id = $2;

-- name: CountTables :one
SELECT count(*) FROM restaurant_tables WHERE restaurant_id = $1;
