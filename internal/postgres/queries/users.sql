-- name: CreateUser :one
INSERT INTO users (tenant_id, email, password_hash, name, role)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- GetUserByEmail is deliberately NOT tenant-scoped: it is how login resolves
-- which tenant the caller belongs to, before any tenant is known. It is the
-- only unscoped read of a tenant-owned table in the codebase. Every other
-- lookup takes the tenant as its first argument.
--
-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1 AND tenant_id = $2;

-- GetUserByID is unscoped, like GetUserByEmail, and for the same reason: token
-- refresh resolves a user from a refresh token that has already proven the
-- caller's identity, before any tenant context exists. Never call this from a
-- request handler — use GetUser, which takes the tenant.
--
-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users
WHERE tenant_id = $1
ORDER BY created_at;

-- name: TouchUserLastLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: SetUserActive :one
UPDATE users
SET is_active = sqlc.arg(is_active)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- Guards the "a tenant must always keep at least one owner" rule, so demoting
-- or deactivating the last owner cannot lock everyone out of billing.
--
-- name: CountActiveOwners :one
SELECT count(*) FROM users
WHERE tenant_id = $1 AND role = 'owner' AND is_active;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1 AND tenant_id = $2;
