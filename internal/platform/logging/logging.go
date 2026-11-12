// Package logging configures log/slog and enriches every record with
// identifiers carried in the context (request id, sync run id, ...).
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

// New returns a logger writing text or JSON records at the given level. The
// handler adds the request id and any attributes stored with WithAttrs from
// the context passed to the *Context logging methods.
func New(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(contextHandler{h})
}

type attrsKey struct{}

// WithAttrs returns a copy of ctx whose log records will include attrs, in
// addition to any attributes already attached to ctx.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, attrsKey{}, merged)
}

type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := requestid.FromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if attrs, ok := ctx.Value(attrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// MaskEmail hides the local part of an email address so logs stay useful for
// correlation without storing PII: "jane.doe@example.com" -> "j***@example.com".
func MaskEmail(email string) string {
	if email == "" {
		return ""
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return "***"
	}
	return local[:1] + "***@" + domain
}
