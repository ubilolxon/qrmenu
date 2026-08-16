// Package httperr defines the single error shape every API surface returns.
//
// Code is a stable machine-readable string the SPA switches on; Message is
// human text it may show directly. Internal failures are deliberately opaque —
// database and driver errors never reach the client.
package httperr

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type envelope struct {
	Error Error `json:"error"`
}

// Common codes. Keep this list authoritative — the SPA depends on the strings.
const (
	CodeBadRequest   = "BAD_REQUEST"
	CodeUnauthorized = "UNAUTHORIZED"
	CodeForbidden    = "FORBIDDEN"
	CodeNotFound     = "NOT_FOUND"
	CodeConflict     = "CONFLICT"
	CodeValidation   = "VALIDATION_FAILED"
	CodeRateLimited  = "RATE_LIMITED"
	CodeInternal     = "INTERNAL"
	CodeUnavailable  = "SERVICE_UNAVAILABLE"
)

// Write sends a structured error response.
func Write(w http.ResponseWriter, status int, code, message string, details any) {
	JSON(w, status, envelope{Error{Code: code, Message: message, Details: details}})
}

// Internal logs the underlying cause and returns a generic message, so driver
// and query details never leak to callers.
func Internal(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "request failed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err),
	)
	Write(w, http.StatusInternalServerError, CodeInternal, "Something went wrong on our side.", nil)
}

func NotFound(w http.ResponseWriter) {
	Write(w, http.StatusNotFound, CodeNotFound, "Resource not found.", nil)
}

// JSON writes v as the response body. A marshalling failure after the status
// line is already sent cannot be reported to the client, so it is only logged.
func JSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		slog.Error("encoding response failed", slog.Any("error", err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// NoContent ends a request that has nothing to return.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }
