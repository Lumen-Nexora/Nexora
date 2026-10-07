package tenant

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/requestctx"
)

type contextKey struct{}
type roleKey struct{}
type modeKey struct{}
type scopesKey struct{}
type apiKeyIDKey struct{}

// WithID attaches a tenant ID to the context.
func WithID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, contextKey{}, tenantID)
}

// IDFromContext returns the tenant ID from context, or empty if unset.
func IDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// WithMode attaches the authenticated environment mode. Request-path services
// must fail closed when it is absent; background jobs rehydrate it from the
// persisted resource before invoking mode-sensitive code.
func WithMode(ctx context.Context, mode domain.Mode) context.Context {
	return context.WithValue(ctx, modeKey{}, mode)
}

// ModeFromContext returns the mode and whether it was present.
func ModeFromContext(ctx context.Context) (domain.Mode, bool) {
	mode, ok := ctx.Value(modeKey{}).(domain.Mode)
	return mode, ok && mode.Valid()
}

// ModeOrDefault is intended only for legacy background jobs whose persisted
// rows predate test mode. HTTP request paths must use ModeFromContext.
func ModeOrDefault(ctx context.Context, fallback domain.Mode) domain.Mode {
	if mode, ok := ModeFromContext(ctx); ok {
		return mode
	}
	return fallback
}

// WithUser attaches a user ID and role to context.
func WithUser(ctx context.Context, userID, role string) context.Context {
	ctx = requestctx.WithUserID(ctx, userID)
	return context.WithValue(ctx, roleKey{}, role)
}

// UserIDFromContext returns the user ID from context, or empty if unset.
func UserIDFromContext(ctx context.Context) string {
	return requestctx.UserID(ctx)
}

// RoleFromContext returns the user role from context, or empty if unset.
func RoleFromContext(ctx context.Context) string {
	role, _ := ctx.Value(roleKey{}).(string)
	return role
}

// WithScopes attaches API key scopes to the context.
func WithScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopesKey{}, scopes)
}

// ScopesFromContext returns the scopes attached to the context, and whether they were set.
func ScopesFromContext(ctx context.Context) ([]string, bool) {
	scopes, ok := ctx.Value(scopesKey{}).([]string)
	return scopes, ok
}

// WithAPIKeyID attaches an API key ID to context.
func WithAPIKeyID(ctx context.Context, keyID string) context.Context {
	return context.WithValue(ctx, apiKeyIDKey{}, keyID)
}

// APIKeyIDFromContext returns the API key ID from context, or empty if unset.
func APIKeyIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(apiKeyIDKey{}).(string)
	return id
}
