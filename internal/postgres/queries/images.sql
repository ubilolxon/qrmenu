-- The row is created before the bytes exist, so the client can be handed a
-- presigned URL to upload to. It stays 'pending' until the server has fetched
-- the object back and decoded it.
--
-- The id is supplied rather than defaulted. It has to be: the storage key is
-- built from it before the row exists, so letting the database pick its own
-- would put the original under one prefix and its derivatives under another —
-- and every prefix-based delete would then silently orphan the original.
--
-- name: CreateImage :one
INSERT INTO images (id, restaurant_id, storage_key, content_type, byte_size)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetImage :one
SELECT * FROM images WHERE id = $1 AND restaurant_id = $2;

-- name: ListImages :many
SELECT * FROM images
WHERE restaurant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- ListReadyImages feeds the public menu. Only 'ready' rows are returned, so a
-- pending or failed upload can never reach a guest's page.
--
-- name: ListReadyImages :many
SELECT * FROM images
WHERE restaurant_id = $1 AND status = 'ready';

-- Promotion is deliberately not guarded on status = 'pending'. Completing twice
-- writes the same derivative keys to the same places, so making the second call
-- a no-success would report a spurious failure for a retry that in fact worked.
--
-- name: MarkImageReady :one
UPDATE images
SET status       = 'ready',
    content_type = sqlc.arg(content_type)::text,
    byte_size    = sqlc.arg(byte_size)::bigint,
    width        = sqlc.arg(width)::integer,
    height       = sqlc.arg(height)::integer,
    placeholder  = sqlc.arg(placeholder)::text,
    variants     = sqlc.arg(variants)::jsonb,
    error        = NULL,
    uploaded_at  = now()
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- A failed row is kept rather than deleted so the client can be told why. The
-- message is user-facing and must not carry internal detail.
--
-- name: MarkImageFailed :one
UPDATE images
SET status = 'failed', error = sqlc.arg(error)::text
WHERE id = sqlc.arg(id) AND restaurant_id = sqlc.arg(restaurant_id)
RETURNING *;

-- Returns the row so the caller knows which storage prefix to clear. Anything
-- pointing at the image is set to NULL by the foreign keys, which is why
-- deleting a photo does not delete the dish.
--
-- name: DeleteImage :one
DELETE FROM images WHERE id = $1 AND restaurant_id = $2
RETURNING *;

-- A presigned upload that was never completed — a tab closed mid-upload, an app
-- that crashed — leaves a pending row and possibly an orphaned object. Both are
-- swept on a timer; without this they accumulate and are billed forever.
--
-- name: ListStalePendingImages :many
SELECT * FROM images
WHERE status = 'pending' AND created_at < sqlc.arg(older_than)::timestamptz
ORDER BY created_at
LIMIT sqlc.arg(max_rows)::integer;

-- name: DeleteImageByID :execrows
DELETE FROM images WHERE id = $1;

-- name: CountImages :one
SELECT count(*) FROM images WHERE restaurant_id = $1;
