// Package service holds the application logic that coordinates the domain
// rules with the database. Handlers stay thin: decode, call, encode.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

type Auth struct {
	store      *postgres.Store
	issuer     *auth.Issuer
	refreshTTL time.Duration
	logger     *slog.Logger

	// decoyHash is verified against when no user matches, so a login attempt
	// for an unknown address costs the same time as one for a known address.
	// Without it, response latency reveals which emails are registered.
	decoyHash string
}

func NewAuth(store *postgres.Store, issuer *auth.Issuer, refreshTTL time.Duration, logger *slog.Logger) (*Auth, error) {
	decoy, err := auth.HashPassword("decoy-password-never-matches")
	if err != nil {
		return nil, fmt.Errorf("prepare login timing decoy: %w", err)
	}
	return &Auth{
		store:      store,
		issuer:     issuer,
		refreshTTL: refreshTTL,
		logger:     logger,
		decoyHash:  decoy,
	}, nil
}

// Session is everything a successful authentication hands back.
type Session struct {
	User             domain.User
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// SessionMeta records where a refresh token was issued, so a user can later be
// shown their active sessions and revoke one.
type SessionMeta struct {
	UserAgent string
	IP        string
}

type RegisterInput struct {
	TenantName string
	Email      string
	Password   string
	Name       string
}

// Register creates a tenant and its first owner together.
//
// Both rows are written in one transaction: a tenant with no owner would be
// unreachable — nobody could log in to it — and orphaned rows like that are
// exactly what a half-applied signup produces.
func (s *Auth) Register(ctx context.Context, in RegisterInput, meta SessionMeta) (*Session, error) {
	in.Email = domain.NormalizeEmail(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	in.TenantName = strings.TrimSpace(in.TenantName)

	var v domain.Validator
	v.Check(in.TenantName != "", "tenant_name", "is required")
	v.Check(len(in.TenantName) <= 200, "tenant_name", "must be at most 200 characters")
	if err := v.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateNewUser(in.Email, in.Password, in.Name, domain.RoleOwner); err != nil {
		return nil, err
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	var created gen.User
	err = s.store.InTx(ctx, func(q *gen.Queries) error {
		tenant, err := q.CreateTenant(ctx, in.TenantName)
		if err != nil {
			return fmt.Errorf("create tenant: %w", postgres.MapError(err))
		}

		created, err = q.CreateUser(ctx, gen.CreateUserParams{
			TenantID:     tenant.ID,
			Email:        in.Email,
			PasswordHash: hash,
			Name:         in.Name,
			Role:         string(domain.RoleOwner),
		})
		if err != nil {
			return fmt.Errorf("create owner: %w", postgres.MapError(err))
		}

		// The free subscription is part of signup, not a later step: every
		// limit check reads one, so a tenant without one is not a free-tier
		// account but a broken row, and the transaction is the only place that
		// can be guaranteed.
		return EnsureSubscription(ctx, q, tenant.ID)
	})
	if err != nil {
		return nil, err
	}

	return s.startSession(ctx, created, meta)
}

// Login verifies credentials and starts a session.
//
// Every failure returns the same error. Distinguishing "no such user" from
// "wrong password" would turn the login form into an account-enumeration oracle.
func (s *Auth) Login(ctx context.Context, email, password string, meta SessionMeta) (*Session, error) {
	email = domain.NormalizeEmail(email)

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			// Spend the same CPU as a real verification before failing.
			_ = auth.VerifyPassword(s.decoyHash, password)
			return nil, domain.ErrUnauthorized
		}
		return nil, fmt.Errorf("look up user: %w", err)
	}

	if err := auth.VerifyPassword(user.PasswordHash, password); err != nil {
		if errors.Is(err, auth.ErrMismatch) {
			return nil, domain.ErrUnauthorized
		}
		return nil, fmt.Errorf("verify password: %w", err)
	}

	if !user.IsActive {
		return nil, domain.Errorf(domain.ErrForbidden, "This account has been deactivated.")
	}

	tenant, err := s.store.GetTenant(ctx, user.TenantID)
	if err != nil {
		return nil, fmt.Errorf("look up tenant: %w", postgres.MapError(err))
	}
	if domain.TenantStatus(tenant.Status) == domain.TenantCancelled {
		return nil, domain.Errorf(domain.ErrForbidden, "This account has been closed.")
	}

	if err := s.store.TouchUserLastLogin(ctx, user.ID); err != nil {
		// Not worth failing a valid login over.
		s.logger.WarnContext(ctx, "could not record last login",
			slog.String("user_id", user.ID.String()), slog.Any("error", err))
	}

	return s.startSession(ctx, user, meta)
}

// Refresh rotates a refresh token: the presented token is consumed and a new
// one from the same family is issued.
//
// Presenting a token that was already consumed means it leaked — the legitimate
// client would have discarded it after rotating. The whole family is revoked in
// that case, which logs the attacker out along with the victim, who then has to
// log in again. That is the intended outcome: a forced re-login beats a
// persistent intruder.
func (s *Auth) Refresh(ctx context.Context, token string, meta SessionMeta) (*Session, error) {
	if token == "" {
		return nil, domain.ErrUnauthorized
	}

	stored, err := s.store.GetRefreshTokenByHash(ctx, auth.HashRefreshToken(token))
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return nil, domain.ErrUnauthorized
		}
		return nil, fmt.Errorf("look up refresh token: %w", err)
	}

	if stored.RevokedAt != nil {
		return nil, domain.ErrUnauthorized
	}
	if time.Now().After(stored.ExpiresAt) {
		return nil, domain.ErrUnauthorized
	}

	if stored.UsedAt != nil {
		s.revokeFamily(ctx, stored.FamilyID, stored.UserID, "refresh token replayed")
		return nil, domain.ErrUnauthorized
	}

	// The atomic step. Two concurrent refreshes with the same token both reach
	// here; the UPDATE's `used_at IS NULL` predicate lets exactly one win.
	consumed, err := s.store.ConsumeRefreshToken(ctx, stored.ID)
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			s.revokeFamily(ctx, stored.FamilyID, stored.UserID, "refresh token used concurrently")
			return nil, domain.ErrUnauthorized
		}
		return nil, fmt.Errorf("consume refresh token: %w", err)
	}

	user, err := s.store.GetUserByID(ctx, consumed.UserID)
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", postgres.MapError(err))
	}
	if !user.IsActive {
		s.revokeFamily(ctx, consumed.FamilyID, consumed.UserID, "user deactivated")
		return nil, domain.ErrUnauthorized
	}

	return s.issueSession(ctx, user, consumed.FamilyID, meta)
}

// Logout revokes the whole family, so every token derived from this login dies
// rather than just the one presented.
func (s *Auth) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil // already logged out; nothing to report
	}

	stored, err := s.store.GetRefreshTokenByHash(ctx, auth.HashRefreshToken(token))
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("look up refresh token: %w", err)
	}

	if _, err := s.store.RevokeRefreshTokenFamily(ctx, stored.FamilyID); err != nil {
		return fmt.Errorf("revoke token family: %w", err)
	}
	return nil
}

// CurrentUser loads the authenticated user. It is tenant-scoped even though
// the user ID alone would be enough to find the row: a token whose tenant claim
// disagrees with the stored row is a token that should not resolve to anything.
func (s *Auth) CurrentUser(ctx context.Context, tenantID, userID uuid.UUID) (domain.User, error) {
	row, err := s.store.GetUser(ctx, gen.GetUserParams{ID: userID, TenantID: tenantID})
	if err != nil {
		return domain.User{}, fmt.Errorf("get user: %w", postgres.MapError(err))
	}
	return postgres.ToUser(row), nil
}

// startSession begins a new rotation family, which is what makes each login a
// separate revocable session.
func (s *Auth) startSession(ctx context.Context, user gen.User, meta SessionMeta) (*Session, error) {
	return s.issueSession(ctx, user, uuid.New(), meta)
}

func (s *Auth) issueSession(ctx context.Context, user gen.User, familyID uuid.UUID, meta SessionMeta) (*Session, error) {
	role := domain.Role(user.Role)

	accessToken, accessExpires, err := s.issuer.IssueAccess(user.ID, user.TenantID, role)
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}

	refreshToken, refreshHash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	refreshExpires := time.Now().Add(s.refreshTTL)

	if _, err := s.store.CreateRefreshToken(ctx, gen.CreateRefreshTokenParams{
		UserID:    user.ID,
		FamilyID:  familyID,
		TokenHash: refreshHash,
		ExpiresAt: refreshExpires,
		UserAgent: nullable(meta.UserAgent),
		Ip:        nullable(meta.IP),
	}); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", postgres.MapError(err))
	}

	return &Session{
		User:             postgres.ToUser(user),
		AccessToken:      accessToken,
		AccessExpiresAt:  accessExpires,
		RefreshToken:     refreshToken,
		RefreshExpiresAt: refreshExpires,
	}, nil
}

// revokeFamily kills a compromised session. A failure here is logged rather
// than returned: the caller is already rejecting the request, and reporting a
// database problem instead of "unauthorized" would leak that the token existed.
func (s *Auth) revokeFamily(ctx context.Context, familyID, userID uuid.UUID, reason string) {
	n, err := s.store.RevokeRefreshTokenFamily(ctx, familyID)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not revoke refresh token family",
			slog.String("family_id", familyID.String()), slog.Any("error", err))
		return
	}
	s.logger.WarnContext(ctx, "revoked refresh token family",
		slog.String("reason", reason),
		slog.String("family_id", familyID.String()),
		slog.String("user_id", userID.String()),
		slog.Int64("tokens_revoked", n),
	)
}

// nullable maps "" to a NULL column rather than storing an empty string.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
