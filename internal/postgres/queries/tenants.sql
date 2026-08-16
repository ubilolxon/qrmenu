-- name: CreateTenant :one
INSERT INTO tenants (name)
VALUES ($1)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- name: UpdateTenantStatus :one
UPDATE tenants
SET status = $2
WHERE id = $1
RETURNING *;
