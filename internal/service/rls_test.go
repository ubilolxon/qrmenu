package service_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/postgres"
)

// Row-level security is the fourth layer of tenant isolation, and it is the one
// that can be completely absent while looking present: a superuser — which is
// what the standard Postgres image makes of POSTGRES_USER — ignores every
// policy, and pg_policies still lists them all.
//
// So these tests connect as an unprivileged role, the way a real deployment
// must, and assert that the policies actually return zero rows. Running them as
// the owner would pass vacuously, which is the exact failure being guarded
// against.

const appRolePassword = "rls-integration-test"

// unprivilegedPool creates the application role if it is missing, grants it what
// deploy/postgres/app-role.sql grants, and returns a pool connected as it.
//
// The role is created here rather than assumed, so this test is self-contained:
// a test that skips unless someone remembered to run a setup script is a test
// that never runs.
func unprivilegedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL or DATABASE_URL to run integration tests")
	}

	ctx := context.Background()
	owner, err := postgres.Connect(ctx, config.DB{URL: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	defer owner.Close()

	const roleName = "qrmenu_rls_test"
	setup := []string{
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'qrmenu_rls_test') THEN
				CREATE ROLE qrmenu_rls_test LOGIN;
			END IF;
		END $$`,
		`ALTER ROLE qrmenu_rls_test NOSUPERUSER NOBYPASSRLS PASSWORD '` + appRolePassword + `'`,
		`GRANT USAGE ON SCHEMA public TO qrmenu_rls_test`,
		`GRANT USAGE ON SCHEMA app TO qrmenu_rls_test`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO qrmenu_rls_test`,
	}
	for _, stmt := range setup {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Skipf("cannot create an unprivileged role (needs a superuser connection): %v", err)
		}
	}

	appDSN, err := withCredentials(dsn, roleName, appRolePassword)
	if err != nil {
		t.Fatalf("build unprivileged DSN: %v", err)
	}

	pool, err := postgres.Connect(ctx, config.DB{URL: appDSN, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("connect as %s: %v", roleName, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func withCredentials(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

// The check the API runs at boot. It has to report false for a superuser, or
// the warning it exists to produce would never fire.
func TestRLSInForceDetectsABypassingRole(t *testing.T) {
	h := newBillingHarness(t) // an owner/superuser pool

	inForce, err := postgres.RLSInForce(context.Background(), h.pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	role, bypasses, err := postgres.DatabaseRole(context.Background(), h.pool)
	if err != nil {
		t.Fatalf("role: %v", err)
	}

	// Whatever the test database's owner happens to be, the two answers must
	// agree: a role that bypasses RLS must not be reported as protected.
	if bypasses && inForce {
		t.Errorf("role %q bypasses RLS but RLSInForce said the policies apply — "+
			"this is the check that stops a deployment believing in a backstop it does not have", role)
	}
	if !bypasses && !inForce {
		t.Errorf("role %q does not bypass RLS but the policies did not apply either", role)
	}
}

func TestRLSInForceForAnUnprivilegedRole(t *testing.T) {
	pool := unprivilegedPool(t)

	inForce, err := postgres.RLSInForce(context.Background(), pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !inForce {
		t.Fatal("the policies did not apply to an unprivileged role — the backstop is decorative")
	}
}

// The assertion the whole migration exists for: a query with no tenant
// predicate at all, which is what a forgotten WHERE looks like, must come back
// empty rather than carrying another tenant's rows.
func TestForgottenPredicateReturnsNothing(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	h.restaurant(t, alice.User.TenantID)
	bob := h.tenant(t)

	pool := unprivilegedPool(t)

	// Deliberately unscoped SQL — no WHERE tenant_id anywhere.
	const leaky = `SELECT count(*) FROM restaurants`

	var asAlice, asBob, asNobody int
	if err := pool.QueryRow(postgres.WithTenant(ctx, alice.User.TenantID), leaky).Scan(&asAlice); err != nil {
		t.Fatalf("query as alice: %v", err)
	}
	if err := pool.QueryRow(postgres.WithTenant(ctx, bob.User.TenantID), leaky).Scan(&asBob); err != nil {
		t.Fatalf("query as bob: %v", err)
	}
	if err := pool.QueryRow(ctx, leaky).Scan(&asNobody); err != nil {
		t.Fatalf("query unscoped: %v", err)
	}

	if asAlice != 1 {
		t.Errorf("alice saw %d restaurants through an unscoped query, want only her own 1", asAlice)
	}
	if asBob != 0 {
		t.Errorf("bob saw %d restaurants, want 0 — he has none", asBob)
	}
	// The unscoped case is permitted by design: it is what migrations, the login
	// lookup and the whole public menu path run as. Asserting it here keeps that
	// deliberate hole visible rather than letting it be discovered later.
	if asNobody <= asAlice {
		t.Errorf("an unscoped connection saw %d restaurants; it is supposed to be unrestricted", asNobody)
	}

	// And the restaurant-scoped tables, which reach their tenant through a
	// subquery rather than a column of their own.
	var bobTables int
	if err := pool.QueryRow(postgres.WithTenant(ctx, bob.User.TenantID),
		`SELECT count(*) FROM restaurant_tables`).Scan(&bobTables); err != nil {
		t.Fatalf("count tables as bob: %v", err)
	}
	if bobTables != 0 {
		t.Errorf("bob saw %d restaurant_tables rows, want 0", bobTables)
	}
}

// A write aimed at another tenant's restaurant must be refused by the database,
// not merely by the service that was supposed to check.
func TestPolicyRefusesAForeignWrite(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	aliceRestaurant := h.restaurant(t, alice.User.TenantID)
	bob := h.tenant(t)

	pool := unprivilegedPool(t)

	// Bob updates Alice's restaurant by id, with no tenant predicate — exactly
	// the shape of a handler that trusted an id from a request body.
	tag, err := pool.Exec(postgres.WithTenant(ctx, bob.User.TenantID),
		`UPDATE restaurants SET phone = '+998000000000' WHERE id = $1`, aliceRestaurant.ID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("bob updated %d of alice's rows — the policy did not apply", tag.RowsAffected())
	}

	// And an insert that names her restaurant.
	_, err = pool.Exec(postgres.WithTenant(ctx, bob.User.TenantID),
		`INSERT INTO restaurant_tables (restaurant_id, label, qr_token) VALUES ($1, 'stolen', 'AAAAAAAAAAAAAA')`,
		aliceRestaurant.ID)
	if err == nil {
		t.Fatal("bob inserted a table into alice's restaurant — WITH CHECK did not apply")
	}
	// A policy violation is its own error; anything else means the insert failed
	// for an unrelated reason and this test proved nothing.
	if !isPolicyViolation(err) {
		t.Fatalf("insert failed, but not because of the policy: %v", err)
	}
}

func isPolicyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	// 42501 is insufficient_privilege, which is what a WITH CHECK failure
	// surfaces as.
	return errors.As(err, &pgErr) && pgErr.SQLState() == "42501"
}

// The GUC must never survive a connection going back to the pool: the next
// request to pick it up may be a public menu read with no tenant at all.
func TestScopeDoesNotLeakBetweenAcquires(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	h.restaurant(t, alice.User.TenantID)

	// One connection, so the second query is guaranteed to reuse the first's.
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL or DATABASE_URL to run integration tests")
	}
	appDSN, err := withCredentials(dsn, "qrmenu_rls_test", appRolePassword)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	unprivilegedPool(t) // ensures the role exists

	single, err := postgres.Connect(ctx, config.DB{URL: appDSN, MaxConns: 1, MinConns: 1})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer single.Close()

	var scoped int
	if err := single.QueryRow(postgres.WithTenant(ctx, alice.User.TenantID),
		`SELECT count(*) FROM restaurants`).Scan(&scoped); err != nil {
		t.Fatalf("scoped query: %v", err)
	}
	if scoped != 1 {
		t.Fatalf("scoped query saw %d restaurants, want 1", scoped)
	}

	// Same physical connection, now used by a request with no tenant. If the
	// setting survived, this would still see only Alice's row.
	var unscoped int
	if err := single.QueryRow(ctx, `SELECT count(*) FROM restaurants`).Scan(&unscoped); err != nil {
		t.Fatalf("unscoped query: %v", err)
	}
	if unscoped == scoped {
		t.Errorf("an unscoped request reused a connection still scoped to a tenant (saw %d) — "+
			"a public menu read would be silently filtered by whoever used the connection last", unscoped)
	}
}
