package transfer

import (
	"context"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

type Repository interface {
	Create(ctx context.Context, tx *domain.Transaction) error
	// CreateWithMonthlyLimit atomically checks the tenant's monthly transfer
	// count and inserts the transaction in a single database transaction,
	// preventing concurrent requests from exceeding the quota.
	CreateWithMonthlyLimit(ctx context.Context, tx *domain.Transaction, tenantID string, year int, month time.Month, limit int) error
	GetByID(ctx context.Context, id string) (*domain.Transaction, error)
	ClaimForSubmission(ctx context.Context, id string) error
	UpdateStatus(ctx context.Context, id string, status domain.TransactionStatus, txHash string) error
	ListByWallet(ctx context.Context, walletID string, limit, offset int) ([]*domain.Transaction, error)
	UpsertByTxHash(ctx context.Context, tx *domain.Transaction) error
	ListByBatch(ctx context.Context, batchID string) ([]*domain.Transaction, error)
	CountMonthlyTransfersByTenant(ctx context.Context, tenantID string, year int, month time.Month) (int, error)
	ExistsByTxHash(ctx context.Context, txHash string) (bool, error)
	GetByIdempotencyKey(ctx context.Context, orgID, idempotencyKey string) (*domain.Transaction, error)
}

// FilterableRepository is implemented by transaction repositories that support multi-field filtering.
type FilterableRepository interface {
	ListWithFilter(ctx context.Context, filter domain.TransactionFilter) ([]*domain.Transaction, error)
}

// IdempotencyRecordRepository is implemented by durable transaction stores that
// can fence a transfer to the exact idempotency generation being recovered.
type IdempotencyRecordRepository interface {
	GetByIdempotencyRecordID(ctx context.Context, recordID string) (*domain.Transaction, error)
}
