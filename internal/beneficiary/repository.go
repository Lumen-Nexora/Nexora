package beneficiary

import (
	"context"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"time"
)

type Repository interface {
	Create(ctx context.Context, beneficiary *domain.Beneficiary) error
	Get(ctx context.Context, id string) (*domain.Beneficiary, error)
	List(ctx context.Context) ([]*domain.Beneficiary, error)
	Check(ctx context.Context, account string) (configured, active bool, err error)
	Activate(ctx context.Context, id string, now time.Time) (*domain.Beneficiary, error)
	Revoke(ctx context.Context, id string) error
}
