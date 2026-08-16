package middleware

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/httperr"
)

const identityKey ctxKey = 1

// Identity is who the request is acting as. It comes only from a verified
// access token — never from a header, query parameter, or request body — which
// is what makes the tenant scope trustworthy enough to put in a WHERE clause.
type Identity struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     domain.Role
}

// IdentityFrom returns the caller's identity. The second result is false on
// unauthenticated routes.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey).(Identity)
	return id, ok
}

// MustIdentity is for handlers mounted behind Authenticate, where a missing
// identity is a wiring bug rather than a client error. Panicking is correct:
// the alternative is a handler that silently runs with a zero-value tenant ID
// and quietly reads nobody's data.
func MustIdentity(ctx context.Context) Identity {
	id, ok := IdentityFrom(ctx)
	if !ok {
		panic("middleware: handler requires authentication but is not mounted behind Authenticate")
	}
	return id
}

// Authenticate verifies the bearer token and attaches the identity.
func Authenticate(issuer *auth.Issuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				httperr.Write(w, http.StatusUnauthorized, httperr.CodeUnauthorized,
					"Authentication required.", nil)
				return
			}

			claims, err := issuer.ParseAccess(token)
			if err != nil {
				// Expired is called out separately so the SPA knows to refresh
				// rather than bounce the user to the login screen.
				if errors.Is(err, auth.ErrExpiredToken) {
					httperr.Write(w, http.StatusUnauthorized, "TOKEN_EXPIRED",
						"Access token has expired.", nil)
					return
				}
				httperr.Write(w, http.StatusUnauthorized, httperr.CodeUnauthorized,
					"Authentication required.", nil)
				return
			}

			userID, err := claims.UserID()
			if err != nil {
				httperr.Write(w, http.StatusUnauthorized, httperr.CodeUnauthorized,
					"Authentication required.", nil)
				return
			}

			identity := Identity{UserID: userID, TenantID: claims.TenantID, Role: claims.Role}
			ctx := context.WithValue(r.Context(), identityKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole restricts a route to an explicit allow-list.
//
// There is no role hierarchy: owner does not automatically satisfy a manager
// check. Listing every permitted role at the route makes access visible where
// it is granted, instead of implied by an ordering somewhere else.
func RequireRole(roles ...domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := IdentityFrom(r.Context())
			if !ok {
				httperr.Write(w, http.StatusUnauthorized, httperr.CodeUnauthorized,
					"Authentication required.", nil)
				return
			}
			if !slices.Contains(roles, identity.Role) {
				httperr.Write(w, http.StatusForbidden, httperr.CodeForbidden,
					"Your role does not allow this action.", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken pulls the credential out of the Authorization header. The scheme
// match is case-insensitive because RFC 7235 says it is.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
