package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// Staff management is where an account can lock itself out or let someone
// promote themselves, so these run against a real Postgres like the rest of the
// isolation suite.

func (h *billingHarness) users() *service.User {
	return service.NewUser(h.store, h.plans)
}

// upgrade puts a tenant on pro, which allows unlimited staff. Used by the
// tests that are about *who* may manage *whom* — on the free plan they would
// otherwise stop after two accounts and be testing the billing ceiling instead
// of the role rules.
func (h *billingHarness) upgrade(t *testing.T, tenantID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	checkout, err := h.billing.Subscribe(ctx, tenantID, "pro", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe to pro: %v", err)
	}
	if _, err := h.billing.MarkPaid(ctx, tenantID, checkout.Invoice.ID, "test"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
}

func staffInput(role domain.Role) service.CreateUserInput {
	return service.CreateUserInput{
		Email:    randomEmail(),
		Password: "correct-horse-battery-staple",
		Name:     "Staff Member",
		Role:     role,
	}
}

// The gap this whole file exists to close: before it, an account had exactly
// one login and there was no way to create the kitchen or waiter accounts the
// staff surface is built for.
func TestOwnerCanCreateFloorStaff(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	h.upgrade(t, owner.User.TenantID)
	users := h.users()

	for _, role := range []domain.Role{domain.RoleManager, domain.RoleWaiter, domain.RoleKitchen} {
		created, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(role))
		if err != nil {
			t.Fatalf("create %s: %v", role, err)
		}
		if created.Role != role {
			t.Errorf("role = %q, want %q", created.Role, role)
		}
		if !created.IsActive {
			t.Errorf("a new %s should start active", role)
		}
	}

	list, err := users.List(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The owner from registration, plus the three just created.
	if len(list) != 4 {
		t.Errorf("tenant has %d users, want 4", len(list))
	}
}

// A manager may hire floor staff — that is the point of letting them reach
// this at all — but must not be able to create or become an owner.
func TestManagerCannotCreateAnOwner(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	h.upgrade(t, owner.User.TenantID)
	users := h.users()

	if _, err := users.Create(ctx, owner.User.TenantID, domain.RoleManager, staffInput(domain.RoleWaiter)); err != nil {
		t.Fatalf("a manager should be able to hire a waiter: %v", err)
	}

	for _, role := range []domain.Role{domain.RoleOwner, domain.RoleManager} {
		_, err := users.Create(ctx, owner.User.TenantID, domain.RoleManager, staffInput(role))
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("a manager creating a %s gave %v, want forbidden", role, err)
		}
	}
}

// The escalation that the role check exists to stop.
func TestManagerCannotPromoteAnyoneToOwner(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	h.upgrade(t, owner.User.TenantID)
	users := h.users()

	waiter, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleWaiter))
	if err != nil {
		t.Fatalf("create waiter: %v", err)
	}
	manager, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleManager))
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	ownerRole := domain.RoleOwner
	_, err = users.Update(ctx, owner.User.TenantID, manager.ID, waiter.ID, domain.RoleManager,
		service.UserPatch{Role: &ownerRole})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a manager promoting a waiter to owner gave %v, want forbidden", err)
	}

	// And they cannot promote themselves, which would make every check above
	// a one-request detour.
	_, err = users.Update(ctx, owner.User.TenantID, manager.ID, manager.ID, domain.RoleManager,
		service.UserPatch{Role: &ownerRole})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a manager promoting themselves gave %v, want forbidden", err)
	}
}

// An account with no active owner has nobody who can reach its billing, add
// staff, or undo the mistake — and no way to get one back.
func TestTheLastOwnerCannotBeRemoved(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	h.upgrade(t, owner.User.TenantID)
	users := h.users()

	second, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleOwner))
	if err != nil {
		t.Fatalf("create second owner: %v", err)
	}

	// Two owners, so demoting one is fine.
	waiterRole := domain.RoleWaiter
	if _, err := users.Update(ctx, owner.User.TenantID, owner.User.ID, second.ID, domain.RoleOwner,
		service.UserPatch{Role: &waiterRole}); err != nil {
		t.Fatalf("demoting one of two owners should be allowed: %v", err)
	}

	// Now there is one, and every route to removing it must refuse. Each is
	// checked separately because they are three different code paths to the
	// same lockout.
	t.Run("demote", func(t *testing.T) {
		role := domain.RoleManager
		_, err := users.Update(ctx, owner.User.TenantID, second.ID, owner.User.ID, domain.RoleOwner,
			service.UserPatch{Role: &role})
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("demoting the last owner gave %v, want conflict", err)
		}
	})
	t.Run("deactivate", func(t *testing.T) {
		_, err := users.SetActive(ctx, owner.User.TenantID, second.ID, owner.User.ID, domain.RoleOwner, false)
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("deactivating the last owner gave %v, want conflict", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		err := users.Delete(ctx, owner.User.TenantID, second.ID, owner.User.ID, domain.RoleOwner)
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("deleting the last owner gave %v, want conflict", err)
		}
	})
}

func TestCannotLockYourselfOut(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	h.upgrade(t, owner.User.TenantID)
	users := h.users()

	// A second owner exists, so the last-owner rule is not what refuses these.
	if _, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleOwner)); err != nil {
		t.Fatalf("create second owner: %v", err)
	}

	id := owner.User.ID
	if _, err := users.SetActive(ctx, owner.User.TenantID, id, id, domain.RoleOwner, false); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("deactivating yourself gave %v, want forbidden", err)
	}
	if err := users.Delete(ctx, owner.User.TenantID, id, id, domain.RoleOwner); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("deleting yourself gave %v, want forbidden", err)
	}
}

// Deactivating must end the sessions the account already holds, or the access
// token in someone's pocket keeps working until it expires.
func TestDeactivationRevokesSessions(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	users := h.users()

	in := staffInput(domain.RoleWaiter)
	waiter, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, in)
	if err != nil {
		t.Fatalf("create waiter: %v", err)
	}

	session, err := h.authSvc.Login(ctx, in.Email, in.Password,
		service.SessionMeta{UserAgent: "test", IP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("waiter login: %v", err)
	}

	if _, err := users.SetActive(ctx, owner.User.TenantID, owner.User.ID, waiter.ID, domain.RoleOwner, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	if _, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{}); err == nil {
		t.Error("a deactivated user's refresh token still worked")
	}
}

// Changing a password ends every other session, because the usual reason for
// changing one is that somebody else knows the old one.
func TestChangingAPasswordEndsOtherSessions(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	users := h.users()

	in := staffInput(domain.RoleManager)
	created, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	session, err := h.authSvc.Login(ctx, in.Email, in.Password, service.SessionMeta{})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// The current password is required even though the caller is already
	// authenticated: an access token may have been left on a shared tablet.
	if err := users.ChangePassword(ctx, created.ID, "not-the-password", "a-brand-new-password"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("wrong current password gave %v, want unauthorized", err)
	}
	if err := users.ChangePassword(ctx, created.ID, in.Password, "short"); err == nil {
		t.Error("a too-short new password was accepted")
	}
	if err := users.ChangePassword(ctx, created.ID, in.Password, "a-brand-new-password"); err != nil {
		t.Fatalf("change password: %v", err)
	}

	if _, err := h.authSvc.Refresh(ctx, session.RefreshToken, service.SessionMeta{}); err == nil {
		t.Error("the old session survived a password change")
	}
	if _, err := h.authSvc.Login(ctx, in.Email, "a-brand-new-password", service.SessionMeta{}); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

// Every method must refuse another tenant's user, and refuse it as "not found"
// rather than "forbidden" — a 403 would confirm the id is real.
func TestUserMethodsRejectForeignTenant(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	alice := h.tenant(t)
	bob := h.tenant(t)
	users := h.users()

	target, err := users.Create(ctx, alice.User.TenantID, domain.RoleOwner, staffInput(domain.RoleWaiter))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	name := "Renamed By Bob"
	operations := map[string]func() error{
		"Get": func() error {
			_, err := users.Get(ctx, bob.User.TenantID, target.ID)
			return err
		},
		"Update": func() error {
			_, err := users.Update(ctx, bob.User.TenantID, bob.User.ID, target.ID, domain.RoleOwner,
				service.UserPatch{Name: &name})
			return err
		},
		"SetActive": func() error {
			_, err := users.SetActive(ctx, bob.User.TenantID, bob.User.ID, target.ID, domain.RoleOwner, false)
			return err
		},
		"ResetPassword": func() error {
			return users.ResetPassword(ctx, bob.User.TenantID, bob.User.ID, target.ID, domain.RoleOwner, "another-password-entirely")
		},
		"Delete": func() error {
			return users.Delete(ctx, bob.User.TenantID, bob.User.ID, target.ID, domain.RoleOwner)
		},
	}

	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("%s across tenants gave %v, want not found", name, err)
			}
		})
	}

	// And Alice's user is untouched by all of that.
	after, err := users.Get(ctx, alice.User.TenantID, target.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Name != "Staff Member" || !after.IsActive {
		t.Errorf("bob changed alice's user: %+v", after)
	}
}

// The plan lever that was advertised on every plan and enforced nowhere.
func TestStaffLimitIsEnforced(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	users := h.users()

	ent, err := h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	// The free plan allows two, and registration already created one.
	if ent.Limits.StaffUsers != 2 {
		t.Fatalf("free plan allows %d staff, this test assumes 2", ent.Limits.StaffUsers)
	}

	if _, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleWaiter)); err != nil {
		t.Fatalf("the second user should fit: %v", err)
	}

	_, err = users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleKitchen))
	if !errors.Is(err, domain.ErrPlanLimit) {
		t.Fatalf("the third user gave %v, want a plan limit", err)
	}

	var limitErr *domain.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("want a *LimitError, got %T", err)
	}
	if limitErr.Resource != domain.ResourceStaffUsers {
		t.Errorf("resource = %q, want staff_users", limitErr.Resource)
	}

	// A deactivated member stops counting, so an account can replace someone
	// without an upgrade.
	list, err := users.List(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var waiterID uuid.UUID
	for _, u := range list {
		if u.Role == domain.RoleWaiter {
			waiterID = u.ID
		}
	}
	if _, err := users.SetActive(ctx, owner.User.TenantID, owner.User.ID, waiterID, domain.RoleOwner, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := users.Create(ctx, owner.User.TenantID, domain.RoleOwner, staffInput(domain.RoleKitchen)); err != nil {
		t.Errorf("a seat freed by deactivation should be reusable: %v", err)
	}
}
