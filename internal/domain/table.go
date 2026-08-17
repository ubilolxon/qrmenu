package domain

import (
	"crypto/rand"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Table struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Label        string
	QRToken      string
	Seats        *int32
	SortOrder    int32
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// QRTokenLength gives 32^14 ≈ 10^21 possible tokens. Guessing one is not a
// realistic attack even with the whole platform's tokens live at once, and it
// still fits comfortably in a scannable QR code.
const QRTokenLength = 14

// Crockford's base32 alphabet: no I, L, O or U. The first three are dropped
// because they are indistinguishable from 1 and 0 when someone reads a code off
// a printed card; U is dropped so the encoding cannot spell unfortunate words.
const tokenAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewQRToken returns an unguessable table token.
//
// It reads uniformly from crypto/rand. The alphabet length is 32, an exact
// power of two, so masking the low 5 bits is unbiased — no modulo skew, and no
// rejection loop needed.
func NewQRToken() (string, error) {
	buf := make([]byte, QRTokenLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = tokenAlphabet[b&31]
	}
	return string(buf), nil
}

// NormalizeQRToken uppercases a token read from a URL, so a hand-typed lowercase
// code still resolves.
func NormalizeQRToken(token string) string {
	return strings.ToUpper(strings.TrimSpace(token))
}

// ValidQRTokenShape reports whether a token could possibly exist, so an
// obviously malformed one is rejected before it reaches the database.
func ValidQRTokenShape(token string) bool {
	if len(token) < 8 || len(token) > 64 {
		return false
	}
	for _, r := range token {
		if !strings.ContainsRune(tokenAlphabet, r) {
			return false
		}
	}
	return true
}

type TableInput struct {
	Label     string
	Seats     *int32
	SortOrder int32
}

func (in TableInput) Validate() error {
	var v Validator

	label := strings.TrimSpace(in.Label)
	v.Check(label != "", "label", "is required")
	v.Check(utf8.RuneCountInString(label) <= 60, "label", "must be at most 60 characters")

	if in.Seats != nil && (*in.Seats < 1 || *in.Seats > 100) {
		v.Add("seats", "must be between 1 and 100")
	}
	validateSortOrder(&v, in.SortOrder)

	return v.Err()
}
