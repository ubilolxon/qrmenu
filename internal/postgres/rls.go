package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Row-level security plumbing.
//
// The policies live in migration 00008. This file is what makes them apply: it
// sets app.tenant_id on the connection before every query that has a tenant, and
// it checks at boot that the setting has any effect at all.

type tenantCtxKey struct{}

// WithTenant marks ctx as belonging to one tenant. The auth middleware calls it
// once the token is verified, so every query made while serving that request is
// scoped by the database as well as by its own WHERE clause.
//
// It is deliberately not called anywhere else. The paths that have no tenant —
// migrations, the login lookup that works out which tenant an address belongs
// to, the whole public menu surface — run unscoped, and the policies are
// written to permit that.
func WithTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFrom returns the tenant ctx was marked with, if any.
func TenantFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(tenantCtxKey{}).(uuid.UUID)
	return id, ok
}

// setTenantGUC runs before every acquire from the pool.
//
// It runs on *every* acquire, including the unscoped ones, and that is the
// whole design: a connection returned to the pool still carries whatever
// setting the last request left on it, so a public menu request that skipped
// this would inherit a previous tenant's scope. Always writing the value —
// empty string when there is none — means a connection can never be handed out
// carrying somebody else's tenant.
//
// One extra round trip per acquire is the cost. It is a single SET against a
// connection that has just been handed over, and the alternative — tracking
// which connection currently holds which tenant — is state that would itself
// have to be correct under concurrency.
func setTenantGUC(ctx context.Context, conn *pgx.Conn) bool {
	value := ""
	if tenantID, ok := TenantFrom(ctx); ok {
		value = tenantID.String()
	}

	// set_config with a parameter, not string interpolation into a SET
	// statement: SET does not take placeholders, so building it by hand would
	// mean concatenating a value into SQL on the hot path. The value is a UUID
	// today and this must stay safe if it ever stops being one.
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", value); err != nil {
		// Returning false destroys this connection and the pool tries another.
		// Failing closed is right: a connection whose scope could not be set is
		// a connection that would run the next query with someone else's.
		return false
	}
	return true
}

// RLSInForce reports whether the policies actually constrain this connection.
//
// It is an empirical check rather than a catalog one, because every way of
// asking the catalog gives an answer that can be true while the protection is
// absent. `relrowsecurity` says the table has RLS; `relforcerowsecurity` says
// the owner is subject to it; neither says anything about the role in use. A
// superuser, or any role with BYPASSRLS, ignores every policy — so a deployment
// connecting as `postgres`, or as the user the standard Postgres image creates
// from POSTGRES_USER, gets a complete and convincing pg_policies listing that
// protects nothing.
//
// So this asks the only question that matters: scoped to a tenant that owns
// nothing, does a table still return rows?
func RLSInForce(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	// A tenant id that cannot exist: uuidv7 values all carry a version nibble,
	// and this one is all zeroes.
	const noSuchTenant = "00000000-0000-0000-0000-000000000000"

	conn, err := pool.Acquire(WithTenant(ctx, uuid.Nil))
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// Set explicitly rather than relying on the acquire hook, so this check
	// still means something if the hook is ever removed or misconfigured.
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", noSuchTenant); err != nil {
		return false, fmt.Errorf("set tenant scope: %w", err)
	}

	var visible int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM restaurants").Scan(&visible); err != nil {
		return false, fmt.Errorf("count restaurants: %w", err)
	}
	return visible == 0, nil
}

// DatabaseRole reports the role the pool authenticates as, and whether that
// role bypasses row-level security. Used to explain a failed RLSInForce check
// in terms an operator can act on.
func DatabaseRole(ctx context.Context, pool *pgxpool.Pool) (role string, bypasses bool, err error) {
	err = pool.QueryRow(ctx, `
		SELECT current_user, rolsuper OR rolbypassrls
		FROM pg_roles WHERE rolname = current_user`).Scan(&role, &bypasses)
	if err != nil {
		return "", false, fmt.Errorf("look up database role: %w", err)
	}
	return role, bypasses, nil
}
