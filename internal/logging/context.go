package logging

import (
	"context"
	"log/slog"
)

// ctxKey is the unexported key type for the per-request logger stored in a
// context by the gRPC interceptors.
type ctxKey struct{}

// IntoContext returns a copy of ctx carrying logger. The gRPC interceptors
// call this after attaching request_id/method/peer so downstream handlers
// pick the contextual logger up via FromContext.
func IntoContext(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the request-scoped logger stored by IntoContext, or
// fallback when none is present. fallback may itself be nil, in which case a
// discard logger is returned so callers never get nil.
func FromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	if fallback != nil {
		return fallback
	}
	return Discard()
}
