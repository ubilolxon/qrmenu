-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at, user_agent, ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- Looked up by hash, never by the token itself — only the SHA-256 is stored.
--
-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1;

-- Marks a token consumed. The WHERE clause makes this the atomic step of
-- rotation: two concurrent refreshes with the same token race here, and exactly
-- one updates a row. The loser gets no row back and is treated as a replay.
--
-- name: ConsumeRefreshToken :one
UPDATE refresh_tokens
SET used_at = now()
WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL
RETURNING *;

-- A replayed token means it leaked, so the entire rotation family dies, not
-- just the one token.
--
-- name: RevokeRefreshTokenFamily :execrows
UPDATE refresh_tokens
SET revoked_at = now()
WHERE family_id = $1 AND revoked_at IS NULL;

-- name: RevokeUserRefreshTokens :execrows
UPDATE refresh_tokens
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens WHERE expires_at < now();
