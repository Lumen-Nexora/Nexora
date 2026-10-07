package batch

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

type Repository interface {
	Create(ctx context.Context, b *domain.Batch) error
	GetByID(ctx context.Context, id string) (*domain.Batch, error)
}
