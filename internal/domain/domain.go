// Package domain holds the business types and rules. It imports no database
// driver, no HTTP package, and no vendor SDK — dependencies point inward, so
// these rules stay testable without infrastructure.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors the service layer returns and the HTTP layer maps to status
// codes. Handlers match on these with errors.Is rather than inspecting driver
// errors.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("already exists")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")

	// ErrPlanLimit is a refusal the customer can act on by paying, not by
	// fixing their request, which is why it is separate from ErrForbidden: the
	// HTTP layer renders it as 402 and the SPA shows an upgrade prompt rather
	// than "you are not allowed to do that".
	ErrPlanLimit = errors.New("plan limit reached")
)

// StatusError pairs a sentinel with a message that is safe to show a user.
//
// Wrapping with fmt.Errorf would produce a message full of internal call
// context ("create restaurant: already exists: ..."), which must never reach a
// client. This keeps the two apart: Unwrap drives the status code, Message is
// the only part rendered.
type StatusError struct {
	Sentinel error
	Message  string
}

func (e *StatusError) Error() string { return e.Sentinel.Error() + ": " + e.Message }
func (e *StatusError) Unwrap() error { return e.Sentinel }

// Errorf builds a StatusError, so errors.Is still matches the sentinel.
func Errorf(sentinel error, format string, args ...any) error {
	return &StatusError{Sentinel: sentinel, Message: fmt.Sprintf(format, args...)}
}

// FieldError names the specific input that was wrong, so the SPA can attach the
// message to the right form field instead of showing a banner.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError carries every problem found in one request, rather than
// failing on the first, so a user fixes the whole form in one round trip.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Validator accumulates field errors.
type Validator struct {
	fields []FieldError
}

func (v *Validator) Add(field, format string, args ...any) {
	v.fields = append(v.fields, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Check adds the error only when ok is false, which keeps call sites to one
// line per rule.
func (v *Validator) Check(ok bool, field, format string, args ...any) {
	if !ok {
		v.Add(field, format, args...)
	}
}

func (v *Validator) OK() bool { return len(v.fields) == 0 }

// Err returns nil when everything passed, so callers can `return v.Err()`.
func (v *Validator) Err() error {
	if v.OK() {
		return nil
	}
	return &ValidationError{Fields: v.fields}
}
