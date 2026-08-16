package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// This is the most important test file in the project.
//
// The tenancy model is shared tables with a tenant_id column, so the only thing
// standing between two restaurants' data is a WHERE clause. Unit tests with a
// fake repository cannot catch a missing predicate — the fake would simply
// return whatever it was told to. These run against a real Postgres and assert
// that tenant B cannot read, change, or delete tenant A's rows through any
// method the service layer exposes.
//
// Every new tenant-scoped method must get a case here.

type harness struct {
	pool        *pgxpool.Pool
	authSvc     *service.Auth
	restaurants *service.Restaurant
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL or DATABASE_URL to run integration tests")
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, config.DB{URL: dsn, MaxConns: 8, MinConns: 1})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	store := postgres.NewStore(pool)
	issuer := auth.NewIssuer([]byte("integration-test-secret-32-bytes!!"), 15*time.Minute, "qrmenu-test")
	logger := slog.New(slog.DiscardHandler)

	authSvc, err := service.NewAuth(store, issuer, 24*time.Hour, logger)
	if err != nil {
		t.Fatalf("build auth service: %v", err)
	}

	return &harness{pool: pool, authSvc: authSvc, restaurants: service.NewRestaurant(store)}
}

// tenant registers a fresh account and returns its owner session. Every test
// makes its own tenants with random identifiers, so tests neither need a clean
// database nor interfere with each other when run in parallel.
func (h *harness) tenant(t *testing.T) *service.Session {
	t.Helper()

	session, err := h.authSvc.Register(context.Background(), service.RegisterInput{
		TenantName: "Test Tenant " + uuid.NewString()[:8],
		Email:      randomEmail(),
		Password:   "correct-horse-battery-staple",
		Name:       "Owner",
	}, service.SessionMeta{UserAgent: "integration-test", IP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("register tenant: %v", err)
	}
	return session
}

func (h *harness) restaurant(t *testing.T, tenantID uuid.UUID) domain.Restaurant {
	t.Helper()

	created, err := h.restaurants.Create(context.Background(), tenantID, domain.RestaurantInput{
		Slug:          randomSlug(),
		Name:          i18n.Text{"uz": "Choyxona", "ru": "Чайхана"},
		Currency:      "UZS",
		Timezone:      "Asia/Tashkent",
		DefaultLocale: "uz",
		Locales:       []string{"uz", "ru"},
	})
	if err != nil {
		t.Fatalf("create restaurant: %v", err)
	}
	return created
}

func randomEmail() string {
	return "u" + strings.ReplaceAll(uuid.NewString(), "-", "") + "@example.test"
}

func randomSlug() string {
	return "r" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
}

// The core assertion: every read, write and delete path refuses a foreign
// tenant, and refuses it as "not found" rather than "forbidden" — a 403 would
// confirm the ID exists, which is itself a leak.
func TestRestaurantMethodsRejectForeignTenant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	bob := h.tenant(t)
	aliceRestaurant := h.restaurant(t, alice.User.TenantID)

	newName := i18n.Text{"uz": "O'zgargan"}
	isActive := false

	operations := map[string]func() error{
		"Get": func() error {
			_, err := h.restaurants.Get(ctx, bob.User.TenantID, aliceRestaurant.ID)
			return err
		},
		"Update": func() error {
			_, err := h.restaurants.Update(ctx, bob.User.TenantID, aliceRestaurant.ID,
				service.RestaurantPatch{Name: &newName})
			return err
		},
		"Update is_active": func() error {
			_, err := h.restaurants.Update(ctx, bob.User.TenantID, aliceRestaurant.ID,
				service.RestaurantPatch{IsActive: &isActive})
			return err
		},
		"Delete": func() error {
			return h.restaurants.Delete(ctx, bob.User.TenantID, aliceRestaurant.ID)
		},
	}

	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			err := op()
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("%s as a foreign tenant returned %v, want ErrNotFound", name, err)
			}
		})
	}

	// The row must be untouched after every attempt above.
	after, err := h.restaurants.Get(ctx, alice.User.TenantID, aliceRestaurant.ID)
	if err != nil {
		t.Fatalf("owner can no longer read their own restaurant: %v", err)
	}
	if after.Name.Get("uz", "uz") != "Choyxona" {
		t.Errorf("name = %q, want it unchanged", after.Name.Get("uz", "uz"))
	}
	if !after.IsActive {
		t.Error("restaurant was deactivated by a foreign tenant")
	}
}

func TestListReturnsOnlyOwnRestaurants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	bob := h.tenant(t)

	aliceOne := h.restaurant(t, alice.User.TenantID)
	aliceTwo := h.restaurant(t, alice.User.TenantID)
	bobOne := h.restaurant(t, bob.User.TenantID)

	aliceList, err := h.restaurants.List(ctx, alice.User.TenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(aliceList) != 2 {
		t.Fatalf("alice sees %d restaurants, want exactly her 2", len(aliceList))
	}

	seen := map[uuid.UUID]bool{}
	for _, r := range aliceList {
		seen[r.ID] = true
		if r.TenantID != alice.User.TenantID {
			t.Errorf("list returned a restaurant belonging to tenant %s", r.TenantID)
		}
	}
	if !seen[aliceOne.ID] || !seen[aliceTwo.ID] {
		t.Error("alice's own restaurants are missing from her list")
	}
	if seen[bobOne.ID] {
		t.Error("alice's list contains bob's restaurant")
	}
}

// The slug is the public menu URL, so uniqueness spans the platform, not the
// tenant. Two tenants must not be able to claim the same one.
func TestSlugIsUniqueAcrossTenants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	bob := h.tenant(t)
	taken := h.restaurant(t, alice.User.TenantID)

	_, err := h.restaurants.Create(ctx, bob.User.TenantID, domain.RestaurantInput{
		Slug:          taken.Slug,
		Name:          i18n.Text{"uz": "Boshqa"},
		Currency:      "UZS",
		Timezone:      "Asia/Tashkent",
		DefaultLocale: "uz",
		Locales:       []string{"uz"},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("claiming another tenant's slug returned %v, want ErrConflict", err)
	}
}

func TestCurrentUserRejectsForeignTenant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	alice := h.tenant(t)
	bob := h.tenant(t)

	// Alice's user ID paired with Bob's tenant must resolve to nothing, even
	// though both halves are individually real.
	_, err := h.authSvc.CurrentUser(ctx, bob.User.TenantID, alice.User.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CurrentUser across tenants returned %v, want ErrNotFound", err)
	}

	if _, err := h.authSvc.CurrentUser(ctx, alice.User.TenantID, alice.User.ID); err != nil {
		t.Fatalf("CurrentUser for the real owner failed: %v", err)
	}
}

func TestRegisterCreatesTenantWithOwner(t *testing.T) {
	h := newHarness(t)

	session := h.tenant(t)

	if session.User.Role != domain.RoleOwner {
		t.Errorf("role = %q, want owner", session.User.Role)
	}
	if session.User.TenantID == uuid.Nil {
		t.Error("tenant id is empty")
	}
	if session.AccessToken == "" || session.RefreshToken == "" {
		t.Error("register did not return a usable session")
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	email := randomEmail()
	input := service.RegisterInput{
		TenantName: "First",
		Email:      email,
		Password:   "correct-horse-battery-staple",
		Name:       "Owner",
	}

	if _, err := h.authSvc.Register(ctx, input, service.SessionMeta{}); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}

	input.TenantName = "Second"
	_, err := h.authSvc.Register(ctx, input, service.SessionMeta{})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate registration returned %v, want ErrConflict", err)
	}

	// The failed signup must not have left a tenant behind with no owner.
	var orphans int
	err = h.pool.QueryRow(ctx, `
		SELECT count(*) FROM tenants t
		WHERE t.name = 'Second'
		  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.tenant_id = t.id)
	`).Scan(&orphans)
	if err != nil {
		t.Fatalf("count orphan tenants: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d tenants were created without an owner; the signup transaction did not roll back", orphans)
	}
}

func TestLoginIsIndistinguishableForUnknownUserAndWrongPassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.tenant(t)

	_, wrongPassword := h.authSvc.Login(ctx, session.User.Email, "not-the-password", service.SessionMeta{})
	_, unknownUser := h.authSvc.Login(ctx, randomEmail(), "not-the-password", service.SessionMeta{})

	if !errors.Is(wrongPassword, domain.ErrUnauthorized) {
		t.Errorf("wrong password returned %v, want ErrUnauthorized", wrongPassword)
	}
	if !errors.Is(unknownUser, domain.ErrUnauthorized) {
		t.Errorf("unknown user returned %v, want ErrUnauthorized", unknownUser)
	}
	// Same error value, so a client cannot tell which case it hit.
	if fmt.Sprint(wrongPassword) != fmt.Sprint(unknownUser) {
		t.Errorf("errors differ and leak which accounts exist: %q vs %q", wrongPassword, unknownUser)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.tenant(t)

	rotated, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rotated.RefreshToken == session.RefreshToken {
		t.Error("refresh returned the same token; it must rotate")
	}
	if rotated.AccessToken == "" {
		t.Error("refresh did not return an access token")
	}

	// The consumed token must not work a second time.
	if _, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("reusing a consumed token returned %v, want ErrUnauthorized", err)
	}
}

// Replay detection: presenting an already-used token means it leaked, so every
// token in that family dies — including the one the legitimate client is
// currently holding.
func TestReplayedRefreshTokenRevokesWholeFamily(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.tenant(t)

	rotated, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{})
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// An attacker replays the stolen original.
	if _, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("replay returned %v, want ErrUnauthorized", err)
	}

	// The legitimate client's current token must now be dead too.
	if _, err := h.authSvc.Refresh(ctx, rotated.RefreshToken, service.SessionMeta{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("token from the compromised family still works: %v", err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.tenant(t)

	if err := h.authSvc.Logout(ctx, session.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("refresh after logout returned %v, want ErrUnauthorized", err)
	}
}

// Logging out of one device must not log out the others: each login starts its
// own rotation family.
func TestLogoutDoesNotAffectOtherSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first := h.tenant(t)

	second, err := h.authSvc.Login(ctx, first.User.Email, "correct-horse-battery-staple", service.SessionMeta{})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	if err := h.authSvc.Logout(ctx, first.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if _, err := h.authSvc.Refresh(ctx, second.RefreshToken, service.SessionMeta{}); err != nil {
		t.Errorf("logging out one device killed another session: %v", err)
	}
}
