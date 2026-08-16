package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
)

var (
	ErrInvalidToken = errors.New("token is invalid")
	ErrExpiredToken = errors.New("token has expired")
)

// Claims carries just enough to authorize a request without a database round
// trip on every call. Anything that can change and must take effect
// immediately — a deactivated user, a role change — is deliberately NOT relied
// on here; that is what the short access-token lifetime is for.
type Claims struct {
	jwt.RegisteredClaims
	TenantID uuid.UUID   `json:"tid"`
	Role     domain.Role `json:"role"`
}

// UserID returns the subject as a UUID.
func (c Claims) UserID() (uuid.UUID, error) {
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("token subject is not a uuid: %w", err)
	}
	return id, nil
}

// Issuer signs and verifies access tokens.
type Issuer struct {
	secret    []byte
	accessTTL time.Duration
	issuer    string
}

func NewIssuer(secret []byte, accessTTL time.Duration, issuerName string) *Issuer {
	return &Issuer{secret: secret, accessTTL: accessTTL, issuer: issuerName}
}

// IssueAccess mints a short-lived bearer token for the SPA to hold in memory.
func (i *Issuer) IssueAccess(userID, tenantID uuid.UUID, role domain.Role) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(i.accessTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    i.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
		TenantID: tenantID,
		Role:     role,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccess verifies the signature and claims.
//
// The algorithm allow-list is what prevents the classic "alg: none" and
// RS256→HS256 confusion attacks, where an attacker picks an algorithm that
// makes the public key double as the signing secret.
func (i *Issuer) ParseAccess(token string) (*Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{},
		func(t *jwt.Token) (any, error) { return i.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(i.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}

	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	if !claims.Role.Valid() {
		return nil, fmt.Errorf("%w: unknown role %q", ErrInvalidToken, claims.Role)
	}
	if claims.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: missing tenant", ErrInvalidToken)
	}
	return claims, nil
}

// RefreshTokenBytes is the entropy in an opaque refresh token. 32 bytes is far
// beyond guessing range and keeps the cookie short.
const RefreshTokenBytes = 32

// NewRefreshToken returns the token to hand to the client and the SHA-256 to
// store. The plaintext is never persisted, so a database leak yields no usable
// sessions.
//
// SHA-256 rather than argon2id is correct here: the token is 256 bits of
// uniform randomness, so there is no dictionary to attack and nothing for a
// slow hash to buy.
func NewRefreshToken() (token string, hash []byte, err error) {
	raw := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken derives the stored lookup key for a token.
func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
