package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/httperr"
)

// MaxBodyBytes caps a JSON request body. Menu payloads with many translations
// are the largest realistic case; image uploads use a different limit.
const MaxBodyBytes = 1 << 20 // 1 MiB

// DecodeJSON reads exactly one JSON object into dst.
//
// Unknown fields are rejected rather than ignored: a client sending "is_admin"
// or a misspelled "default_local" should be told, not silently obeyed in part.
// The body is also length-limited and required to contain a single value, so a
// malformed or oversized request cannot exhaust memory.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, _ := strings.Cut(ct, ";")
		if strings.TrimSpace(mediaType) != "application/json" {
			return domain.Errorf(errBadRequest, "Content-Type must be application/json.")
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value means the body was "{...} {...}", which is never intended.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.Errorf(errBadRequest, "Body must contain a single JSON object.")
	}
	return nil
}

// errBadRequest is local to the HTTP layer: malformed JSON is a transport
// problem, not a domain concept, so it does not belong in package domain.
var errBadRequest = errors.New("bad request")

func decodeError(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var maxBytesErr *http.MaxBytesError

	switch {
	case errors.As(err, &syntaxErr):
		return domain.Errorf(errBadRequest, "Body contains malformed JSON at character %d.", syntaxErr.Offset)

	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return domain.Errorf(errBadRequest, "Field %q has the wrong type.", typeErr.Field)
		}
		return domain.Errorf(errBadRequest, "Body contains a value of the wrong type.")

	case errors.As(err, &maxBytesErr):
		return domain.Errorf(errBadRequest, "Body must be no larger than %d bytes.", maxBytesErr.Limit)

	case errors.Is(err, io.EOF):
		return domain.Errorf(errBadRequest, "Body must not be empty.")

	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return domain.Errorf(errBadRequest, "Body contains an unknown field %s.", field)

	default:
		return domain.Errorf(errBadRequest, "Body could not be parsed.")
	}
}

// RenderError maps a service error onto a status code and response body.
//
// The default case is deliberately a 500 with an opaque message: an error this
// function does not recognise is a bug, and guessing a friendlier status for it
// would hide the bug behind a plausible-looking 400.
func RenderError(w http.ResponseWriter, r *http.Request, err error) {
	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		httperr.Write(w, http.StatusUnprocessableEntity, httperr.CodeValidation,
			"Some fields are invalid.", validationErr.Fields)
		return
	}

	switch {
	case errors.Is(err, domain.ErrNotFound):
		httperr.Write(w, http.StatusNotFound, httperr.CodeNotFound,
			message(err, "Resource not found."), nil)

	case errors.Is(err, domain.ErrConflict):
		httperr.Write(w, http.StatusConflict, httperr.CodeConflict,
			message(err, "That record already exists."), nil)

	case errors.Is(err, domain.ErrUnauthorized):
		httperr.Write(w, http.StatusUnauthorized, httperr.CodeUnauthorized,
			message(err, "Invalid credentials."), nil)

	case errors.Is(err, domain.ErrForbidden):
		httperr.Write(w, http.StatusForbidden, httperr.CodeForbidden,
			message(err, "You are not allowed to do that."), nil)

	case errors.Is(err, errBadRequest):
		httperr.Write(w, http.StatusBadRequest, httperr.CodeBadRequest,
			message(err, "Request could not be processed."), nil)

	default:
		httperr.Internal(w, r, err)
	}
}

// message returns the user-safe text from a StatusError, or fallback when the
// error carries only a sentinel. Raw error strings are never rendered — they
// contain internal call context.
func message(err error, fallback string) string {
	var statusErr *domain.StatusError
	if errors.As(err, &statusErr) && statusErr.Message != "" {
		return statusErr.Message
	}
	return fallback
}

// PathUUID parses a UUID path parameter, reporting a 400 rather than letting a
// malformed ID reach the database as a failed query.
func PathUUID(r *http.Request, param string, value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, domain.Errorf(errBadRequest, "%s must be a valid UUID.", param)
	}
	return id, nil
}

// isNotFound reports whether an error should render as "no such thing", used by
// the HTML surface which needs a page rather than a JSON envelope.
func isNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}
