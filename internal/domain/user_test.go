package domain_test

import (
	"testing"

	"github.com/ubilolxon/qrmenu/internal/domain"
)

// Who may hand out which job. The rules are a pair — actor and target — because
// every interesting mistake is about the combination, not about a rank.
func TestRoleCanManageUser(t *testing.T) {
	allowed := []struct{ actor, target domain.Role }{
		{domain.RoleOwner, domain.RoleOwner},
		{domain.RoleOwner, domain.RoleManager},
		{domain.RoleOwner, domain.RoleWaiter},
		{domain.RoleOwner, domain.RoleKitchen},
		// A manager runs the floor, so hiring floor staff is part of the job.
		{domain.RoleManager, domain.RoleWaiter},
		{domain.RoleManager, domain.RoleKitchen},
	}
	for _, tc := range allowed {
		if !domain.RoleCanManageUser(tc.actor, tc.target) {
			t.Errorf("%s should be able to manage a %s", tc.actor, tc.target)
		}
	}

	refused := []struct {
		actor, target domain.Role
		why           string
	}{
		{domain.RoleManager, domain.RoleOwner, "a manager must not be able to create or change an owner"},
		{domain.RoleManager, domain.RoleManager, "a manager promoting a peer is how one becomes several"},
		{domain.RoleWaiter, domain.RoleWaiter, "a waiter manages nobody"},
		{domain.RoleWaiter, domain.RoleKitchen, "a waiter manages nobody"},
		{domain.RoleKitchen, domain.RoleWaiter, "the kitchen manages nobody"},
		{domain.RoleKitchen, domain.RoleOwner, "the kitchen manages nobody"},
	}
	for _, tc := range refused {
		if domain.RoleCanManageUser(tc.actor, tc.target) {
			t.Errorf("%s -> %s should be refused: %s", tc.actor, tc.target, tc.why)
		}
	}
}

// An unknown role must manage nobody and be manageable by nobody, rather than
// falling through to a permissive default.
func TestUnknownRoleManagesNothing(t *testing.T) {
	ghost := domain.Role("superadmin")

	if domain.RoleCanManageUser(ghost, domain.RoleWaiter) {
		t.Error("an unknown role was allowed to manage a waiter")
	}
	if domain.RoleCanManageUser(domain.RoleOwner, ghost) {
		t.Error("an owner was allowed to hand out a role that does not exist")
	}
}
