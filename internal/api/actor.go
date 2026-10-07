package api

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/requestctx"
)

// ActorFromContext returns the authenticated user ID that performed the
// request, or "" for unattributed calls (API keys, internal jobs). Admin
// endpoints include it in audit logs so an out-of-band action can be traced
// back to a person.
func ActorFromContext(ctx context.Context) string {
	return requestctx.UserID(ctx)
}
