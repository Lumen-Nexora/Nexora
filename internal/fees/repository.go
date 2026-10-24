package fees

import (
	"context"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

type Repository interface {
	GetSchedule(ctx context.Context, tenantID *string, asset string) (*domain.FeeSchedule, error)
	SetSchedule(ctx context.Context, schedule *domain.FeeSchedule) error
	RecordCollection(ctx context.Context, collection *domain.FeeCollection) error
	ListCollected(ctx context.Context, start, end *time.Time, tenantID *string, limit, offset int) ([]*domain.FeeCollection, error)
	GetMonthlyVolume(ctx context.Context, tenantID string) (decimal.Decimal, error)
	GetApplicableTier(ctx context.Context, tenantID string, volume decimal.Decimal) *domain.FeeTier

	// RecordNetworkFee persists one exact on-chain fee. It is idempotent on
	// tx_hash: replaying the same Horizon result never allocates the cost twice.
	RecordNetworkFee(ctx context.Context, charge *domain.NetworkFeeCharge) error
	GetNetworkFeeByTxHash(ctx context.Context, txHash string) (*domain.NetworkFeeCharge, error)
	GetNetworkFeeUsage(ctx context.Context, tenantID string, start, end time.Time) (*domain.NetworkFeeUsage, error)
	GetMonthlyNetworkFeeCOGS(ctx context.Context, start, end time.Time) (*domain.NetworkFeeCOGSReport, error)
}

type TransferFee struct {
	FeeAmount decimal.Decimal
	NetAmount decimal.Decimal
	FeeBps    int
}

type Service interface {
	GetSchedule(ctx context.Context, tenantID string) (*domain.FeeSchedule, error)
	SetSchedule(ctx context.Context, schedule *domain.FeeSchedule) error
	CalculateTransferFee(ctx context.Context, tenantID, asset string, amount decimal.Decimal) (*TransferFee, error)
	CalculateConversionFee(ctx context.Context, tenantID, asset string, amount decimal.Decimal) (*TransferFee, error)
	RecordCollection(ctx context.Context, collection *domain.FeeCollection) error
	ListCollected(ctx context.Context, start, end *time.Time, tenantID *string, limit, offset int) ([]*domain.FeeCollection, error)
}
