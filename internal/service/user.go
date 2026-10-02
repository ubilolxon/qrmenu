package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// User manages the people who work in a restaurant.
//
// Until this existed an account had exactly one login — the owner created at
// registration — which made the whole staff surface theoretical: the kitchen
// screen and the order board are built for a kitchen and a waiter, and there
// was no way to create either.
//
// There is no email invitation, because there is no mail infrastructure yet.
// The owner sets an initial password and hands it over; ChangePassword is how
// the member replaces it with something the owner does not know.
type User struct {
	store *postgres.Store
	plans *Plan
}

// plans may be nil, which permits everything. See service.Plan.
func NewUser(store *postgres.Store, plans *Plan) *User {
	return &User{store: store, plans: plans}
}

type CreateUserInput struct {
	Email    string
	Password string
	Name     string
	Role     domain.Role
}

// Create adds a member of staff.
//
// actor is the role of the person doing it, and it is checked against the role
// being handed out rather than against a rank: a manager may hire a waiter and
// may not create an owner. See domain.RoleCanManageUser.
func (s *User) Create(ctx context.Context, tenantID uuid.UUID, actor domain.Role, in CreateUserInput) (domain.User, error) {
	in.Email = domain.NormalizeEmail(in.Email)
	in.Name = strings.TrimSpace(in.Name)

	if err := domain.ValidateNewUser(in.Email, in.Password, in.Name, in.Role); err != nil {
		return domain.User{}, err
	}
	if !domain.RoleCanManageUser(actor, in.Role) {
		return domain.User{}, domain.Errorf(domain.ErrForbidden,
			"A %s may not create a %s.", actor, in.Role)
	}
	// After the role check, so someone who is not allowed to add staff at all
	// is told that rather than sent to the pricing page.
	if err := s.plans.Check(ctx, tenantID, domain.ResourceStaffUsers); err != nil {
		return domain.User{}, err
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return domain.User{}, fmt.Errorf("hash password: %w", err)
	}

	row, err := s.store.CreateUser(ctx, gen.CreateUserParams{
		TenantID:     tenantID,
		Email:        in.Email,
		PasswordHash: hash,
		Name:         in.Name,
		Role:         string(in.Role),
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("create user: %w", postgres.MapError(err))
	}
	return postgres.ToUser(row), nil
}

func (s *User) List(ctx context.Context, tenantID uuid.UUID) ([]domain.User, error) {
	rows, err := s.store.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", postgres.MapError(err))
	}
	return postgres.ToUsers(rows), nil
}

func (s *User) Get(ctx context.Context, tenantID, id uuid.UUID) (domain.User, error) {
	row, err := s.store.GetUser(ctx, gen.GetUserParams{ID: id, TenantID: tenantID})
	if err != nil {
		return domain.User{}, fmt.Errorf("get user: %w", postgres.MapError(err))
	}
	return postgres.ToUser(row), nil
}

// UserPatch carries only what the caller sent. A nil pointer means "leave this
// alone", which is what makes PATCH different from PUT.
type UserPatch struct {
	Name *string
	Role *domain.Role
}

// Update changes a member's name or job.
//
// Three refusals are worth naming, because each is a way an account could lock
// itself out or escalate:
//
//   - You cannot change your own role. Otherwise a manager promotes themselves
//     and the role check above is decoration.
//   - You cannot touch someone whose current role you may not manage, nor hand
//     out a role you may not manage. Both ends, or a manager promotes a waiter
//     to owner.
//   - You cannot remove the last active owner. An account with no owner has
//     nobody who can reach its billing, and no way to get one back.
func (s *User) Update(ctx context.Context, tenantID, actorID, id uuid.UUID, actor domain.Role, patch UserPatch) (domain.User, error) {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return domain.User{}, err
	}

	if patch.Role != nil && *patch.Role != current.Role {
		if id == actorID {
			return domain.User{}, domain.Errorf(domain.ErrForbidden,
				"You cannot change your own role.")
		}
		if !domain.RoleCanManageUser(actor, current.Role) || !domain.RoleCanManageUser(actor, *patch.Role) {
			return domain.User{}, domain.Errorf(domain.ErrForbidden,
				"A %s may not change a %s into a %s.", actor, current.Role, *patch.Role)
		}
		if !(*patch.Role).Valid() {
			return domain.User{}, domain.Errorf(domain.ErrNotFound, "%q is not a role.", *patch.Role)
		}
		if current.Role == domain.RoleOwner {
			if err := s.assertNotLastOwner(ctx, tenantID, id); err != nil {
				return domain.User{}, err
			}
		}
	} else if !domain.RoleCanManageUser(actor, current.Role) {
		return domain.User{}, domain.Errorf(domain.ErrForbidden,
			"A %s may not change a %s.", actor, current.Role)
	}

	params := gen.UpdateUserParams{ID: id, TenantID: tenantID}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		var v domain.Validator
		v.Check(name != "", "name", "is required")
		v.Check(len(name) <= 200, "name", "must be at most 200 characters")
		if err := v.Err(); err != nil {
			return domain.User{}, err
		}
		params.Name = &name
	}
	if patch.Role != nil {
		role := string(*patch.Role)
		params.Role = &role
	}

	row, err := s.store.UpdateUser(ctx, params)
	if err != nil {
		return domain.User{}, fmt.Errorf("update user: %w", postgres.MapError(err))
	}
	return postgres.ToUser(row), nil
}

// SetActive enables or disables a login.
//
// Deactivating rather than deleting is the normal case: a waiter who leaves
// should stop being able to sign in, but the orders they accepted still name
// them. Every session they hold is revoked at the same time, or the access
// token they already have keeps working until it expires.
func (s *User) SetActive(ctx context.Context, tenantID, actorID, id uuid.UUID, actor domain.Role, active bool) (domain.User, error) {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return domain.User{}, err
	}
	if id == actorID && !active {
		return domain.User{}, domain.Errorf(domain.ErrForbidden,
			"You cannot deactivate your own account.")
	}
	if !domain.RoleCanManageUser(actor, current.Role) {
		return domain.User{}, domain.Errorf(domain.ErrForbidden,
			"A %s may not deactivate a %s.", actor, current.Role)
	}
	if !active && current.Role == domain.RoleOwner {
		if err := s.assertNotLastOwner(ctx, tenantID, id); err != nil {
			return domain.User{}, err
		}
	}

	var updated gen.User
	err = s.store.InTx(ctx, func(q *gen.Queries) error {
		var err error
		updated, err = q.SetUserActive(ctx, gen.SetUserActiveParams{
			ID: id, TenantID: tenantID, IsActive: active,
		})
		if err != nil {
			return fmt.Errorf("set user active: %w", postgres.MapError(err))
		}
		if active {
			return nil
		}
		// Revoking inside the same transaction, so a deactivation can never
		// half-apply and leave a disabled account with a live session.
		if _, err := q.RevokeUserRefreshTokens(ctx, id); err != nil {
			return fmt.Errorf("revoke sessions: %w", postgres.MapError(err))
		}
		return nil
	})
	if err != nil {
		return domain.User{}, err
	}
	return postgres.ToUser(updated), nil
}

// ChangePassword replaces the caller's own password.
//
// The current one is required even though the caller is already authenticated:
// an access token is bearer credential that may have been left on a shared
// tablet, and "someone walked past an unlocked screen" should not be enough to
// take an account over. Every other session is revoked, which is what makes
// this useful after a password has been shared and regretted.
func (s *User) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	var v domain.Validator
	v.Check(current != "", "current_password", "is required")
	if err := v.Err(); err != nil {
		return err
	}
	if err := domain.ValidateCredentials("placeholder@example.com", next); err != nil {
		// Only the password half can fail here; the address is a stand-in so
		// the shared validator can be reused rather than duplicated.
		var validationErr *domain.ValidationError
		if errors.As(err, &validationErr) {
			for _, f := range validationErr.Fields {
				if f.Field == "password" {
					return &domain.ValidationError{Fields: []domain.FieldError{
						{Field: "new_password", Message: f.Message},
					}}
				}
			}
		}
		return err
	}

	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", postgres.MapError(err))
	}
	if err := auth.VerifyPassword(user.PasswordHash, current); err != nil {
		if errors.Is(err, auth.ErrMismatch) {
			return domain.Errorf(domain.ErrUnauthorized, "Current password is incorrect.")
		}
		return fmt.Errorf("verify password: %w", err)
	}

	hash, err := auth.HashPassword(next)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	return s.store.InTx(ctx, func(q *gen.Queries) error {
		if _, err := q.UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{
			ID: userID, PasswordHash: hash,
		}); err != nil {
			return fmt.Errorf("update password: %w", postgres.MapError(err))
		}
		// Changing a password is the one action that should end every other
		// session, because the usual reason for it is that someone else knows
		// the old one.
		if _, err := q.RevokeUserRefreshTokens(ctx, userID); err != nil {
			return fmt.Errorf("revoke sessions: %w", postgres.MapError(err))
		}
		return nil
	})
}

// ResetPassword lets an owner set a new password for someone who has forgotten
// theirs. With no mail infrastructure there is no self-service reset, and the
// alternative to this is an account nobody can get back into.
func (s *User) ResetPassword(ctx context.Context, tenantID, actorID, id uuid.UUID, actor domain.Role, password string) error {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if id == actorID {
		return domain.Errorf(domain.ErrForbidden,
			"Use the change-password endpoint for your own account.")
	}
	if !domain.RoleCanManageUser(actor, current.Role) {
		return domain.Errorf(domain.ErrForbidden,
			"A %s may not reset a %s's password.", actor, current.Role)
	}
	if err := domain.ValidateCredentials(current.Email, password); err != nil {
		return err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	return s.store.InTx(ctx, func(q *gen.Queries) error {
		if _, err := q.UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{
			ID: id, PasswordHash: hash,
		}); err != nil {
			return fmt.Errorf("update password: %w", postgres.MapError(err))
		}
		if _, err := q.RevokeUserRefreshTokens(ctx, id); err != nil {
			return fmt.Errorf("revoke sessions: %w", postgres.MapError(err))
		}
		return nil
	})
}

// Delete removes a login outright. SetActive is almost always the right
// choice; this exists for a row created by mistake.
func (s *User) Delete(ctx context.Context, tenantID, actorID, id uuid.UUID, actor domain.Role) error {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if id == actorID {
		return domain.Errorf(domain.ErrForbidden, "You cannot delete your own account.")
	}
	if !domain.RoleCanManageUser(actor, current.Role) {
		return domain.Errorf(domain.ErrForbidden, "A %s may not delete a %s.", actor, current.Role)
	}
	if current.Role == domain.RoleOwner {
		if err := s.assertNotLastOwner(ctx, tenantID, id); err != nil {
			return err
		}
	}

	n, err := s.store.DeleteUser(ctx, gen.DeleteUserParams{ID: id, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("delete user: %w", postgres.MapError(err))
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// assertNotLastOwner refuses the change that would leave an account with no
// active owner — and therefore with nobody who can reach its billing, add
// staff, or undo the mistake.
func (s *User) assertNotLastOwner(ctx context.Context, tenantID, excluding uuid.UUID) error {
	others, err := s.store.CountOtherActiveOwners(ctx, gen.CountOtherActiveOwnersParams{
		TenantID: tenantID, ID: excluding,
	})
	if err != nil {
		return fmt.Errorf("count owners: %w", postgres.MapError(err))
	}
	if others == 0 {
		return domain.Errorf(domain.ErrConflict,
			"This is the account's only active owner. Make someone else an owner first.")
	}
	return nil
}
