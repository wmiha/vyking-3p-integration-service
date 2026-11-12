// Package requestid generates, validates and carries request identifiers.
// Every response carries one so that partner calls can be correlated with our
// logs and, later, with downstream services.
package requestid

import (
	"context"
	"uuid"
)

// Header is the HTTP header used to accept and return request identifiers.
const Header = "X-Request-ID"

// maxLen bounds accepted inbound identifiers to keep logs and headers sane.
const maxLen = 64

type ctxKey struct{}

// New returns a time-ordered UUIDv7, which sorts naturally in logs.
func New() string {
	return uuid.NewV7().String()
}

// NewContext returns a copy of ctx carrying id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request id stored in ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Valid reports whether an inbound id may be propagated as-is. Only short
// tokens of [A-Za-z0-9._-] are accepted, which prevents header and log
// injection through client-supplied values.
func Valid(id string) bool {
	if id == "" || len(id) > maxLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
