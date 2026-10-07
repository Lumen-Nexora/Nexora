package claimable

import (
	"context"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// Repository persists claimable balances, scoped to the tenant on the context
// where one is present (the API path) and unscoped where it is not (the expiry
// worker, which sweeps every tenant).
type Repository interface {
	Create(ctx context.Context, b *domain.ClaimableBalance) error
	GetByID(ctx context.Context, id string) (*domain.ClaimableBalance, error)
	List(ctx context.Context, f domain.ClaimableFilter) ([]*domain.ClaimableBalance, error)
	MarkClaimed(ctx context.Context, id, claimedBy string, at time.Time) error
	MarkExpired(ctx context.Context, id string, at time.Time) error
	MarkRevoked(ctx context.Context, id string, at time.Time) error
	// ListExpiredPending returns pending balances whose expires_at has passed,
	// across every tenant, for the background tracker.
	ListExpiredPending(ctx context.Context, now time.Time, limit int) ([]*domain.ClaimableBalance, error)
}
