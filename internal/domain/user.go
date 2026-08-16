package domain

import (
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Role determines what a user may do. There is deliberately no ranking between
// roles: waiter and kitchen are not "less than" manager, they are different
// jobs. Permission checks use explicit allow-lists so a new role cannot silently
// inherit access by sorting above something.
type Role string

const (
	RoleOwner   Role = "owner"   // billing and everything below
	RoleManager Role = "manager" // menu and orders, no billing
	RoleWaiter  Role = "waiter"  // orders only
	RoleKitchen Role = "kitchen" // order status only
)

var allRoles = []Role{RoleOwner, RoleManager, RoleWaiter, RoleKitchen}

func (r Role) Valid() bool { return slices.Contains(allRoles, r) }

func (r Role) String() string { return string(r) }

// AllRoles returns every role, for validation messages and admin UIs.
func AllRoles() []Role { return slices.Clone(allRoles) }

type TenantStatus string

const (
	TenantActive    TenantStatus = "active"
	TenantSuspended TenantStatus = "suspended"
	TenantCancelled TenantStatus = "cancelled"
)

type Tenant struct {
	ID        uuid.UUID
	Name      string
	Status    TenantStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CanServe reports whether the tenant's restaurants should still answer public
// requests. A cancelled account goes dark; a suspended one is read-only, which
// is handled at the write path rather than here.
func (t Tenant) CanServe() bool { return t.Status != TenantCancelled }

type User struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Email       string
	Name        string
	Role        Role
	IsActive    bool
	LastLoginAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const (
	// MinPasswordLen follows current NIST guidance: length is what matters,
	// so there are no composition rules to memorise.
	MinPasswordLen = 10
	// MaxPasswordLen bounds the argon2id input. Hashing is deliberately
	// expensive, so an unbounded password is a cheap way to burn server CPU.
	MaxPasswordLen = 200
)

// NormalizeEmail lowercases and trims. The column is citext so the database
// compares case-insensitively too; this keeps what is stored tidy.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateCredentials checks a new user's email and password.
func ValidateCredentials(email, password string) error {
	var v Validator
	validateEmail(&v, email)
	validatePassword(&v, password)
	return v.Err()
}

func validateEmail(v *Validator, email string) {
	switch {
	case email == "":
		v.Add("email", "is required")
	case len(email) > 254:
		v.Add("email", "is too long")
	default:
		if _, err := mail.ParseAddress(email); err != nil {
			v.Add("email", "is not a valid email address")
		}
	}
}

func validatePassword(v *Validator, password string) {
	// Count runes, not bytes: a 10-character Cyrillic password is 20 bytes and
	// would otherwise pass a byte-length check that a Latin one fails.
	n := utf8.RuneCountInString(password)
	switch {
	case password == "":
		v.Add("password", "is required")
	case n < MinPasswordLen:
		v.Add("password", "must be at least %d characters", MinPasswordLen)
	case n > MaxPasswordLen:
		v.Add("password", "must be at most %d characters", MaxPasswordLen)
	}
}

// ValidateNewUser checks everything needed to create a user.
func ValidateNewUser(email, password, name string, role Role) error {
	var v Validator
	validateEmail(&v, email)
	validatePassword(&v, password)

	name = strings.TrimSpace(name)
	v.Check(name != "", "name", "is required")
	v.Check(utf8.RuneCountInString(name) <= 200, "name", "must be at most 200 characters")

	v.Check(role.Valid(), "role", "must be one of owner, manager, waiter, kitchen")

	return v.Err()
}
