// Package apierror is the single place that defines how errors are presented
// to API clients. Every failure response carries a stable, machine-readable
// code and the request id:
//
//	{"error": {"code": "customer_not_found", "message": "...", "request_id": "...", "details": {...}}}
//
// Internal causes are kept for logging only and never serialized.
package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

// Code is a stable, machine-readable error identifier. Codes are part of the
// public contract: never rename one, only add new ones.
type Code string

const (
	CodeInvalidParameter   Code = "invalid_parameter"
	CodeInvalidCursor      Code = "invalid_cursor"
	CodeUnauthorized       Code = "unauthorized"
	CodeTokenMalformed     Code = "token_malformed"
	CodeTokenExpired       Code = "token_expired"
	CodeTokenInvalid       Code = "token_invalid"
	CodeInsufficientScope  Code = "insufficient_scope"
	CodeBrandForbidden     Code = "brand_forbidden"
	CodeCustomerNotFound   Code = "customer_not_found"
	CodeCustomerAmbiguous  Code = "customer_ambiguous"
	CodeNotFound           Code = "not_found"
	CodeMethodNotAllowed   Code = "method_not_allowed"
	CodeServiceUnavailable Code = "service_unavailable"
	CodeInternal           Code = "internal_error"
)

// Error is an error that can be rendered to an API client.
type Error struct {
	Status  int
	Code    Code
	Message string
	Details map[string]any
	// Err is the internal cause, logged but never exposed to clients.
	Err error
}

// New returns an Error with the given HTTP status, code and client-facing message.
func New(status int, code Code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s (%d): %s: %v", e.Code, e.Status, e.Message, e.Err)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// WithDetails returns a copy of e with additional client-visible details.
func (e *Error) WithDetails(details map[string]any) *Error {
	c := *e
	c.Details = maps.Clone(e.Details)
	if c.Details == nil {
		c.Details = make(map[string]any, len(details))
	}
	maps.Copy(c.Details, details)
	return &c
}

// WithCause returns a copy of e carrying an internal cause for logging.
func (e *Error) WithCause(err error) *Error {
	c := *e
	c.Err = err
	return &c
}

// InvalidParameter reports a malformed or out-of-range request parameter.
func InvalidParameter(param, message string) *Error {
	return New(http.StatusBadRequest, CodeInvalidParameter, message).
		WithDetails(map[string]any{"parameter": param})
}

// Internal hides err behind a generic message; err is only logged.
func Internal(err error) *Error {
	return New(http.StatusInternalServerError, CodeInternal, "internal server error").WithCause(err)
}

// Unavailable reports a temporary dependency failure (database, cache, ...).
func Unavailable(err error) *Error {
	return New(http.StatusServiceUnavailable, CodeServiceUnavailable,
		"service temporarily unavailable, retry later").WithCause(err)
}

type envelope struct {
	Error body `json:"error"`
}

type body struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Write renders err as a JSON error response. Errors that are not (or do not
// wrap) an *Error are treated as internal errors. Server-side failures are
// logged together with their cause.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	apiErr, ok := errors.AsType[*Error](err)
	if !ok {
		apiErr = Internal(err)
	}

	ctx := r.Context()
	if apiErr.Status >= http.StatusInternalServerError {
		slog.ErrorContext(ctx, "request failed",
			"status", apiErr.Status, "code", apiErr.Code, "error", err)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(apiErr.Status)
	_ = json.NewEncoder(w).Encode(envelope{Error: body{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		RequestID: requestid.FromContext(ctx),
		Details:   apiErr.Details,
	}})
}
