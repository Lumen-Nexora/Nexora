package wallet

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

type Repository interface {
	Create(ctx context.Context, w *domain.Wallet) error
	GetByID(ctx context.Context, id string) (*domain.Wallet, error)
	GetByPublicKey(ctx context.Context, pubKey string) (*domain.Wallet, error)
	List(ctx context.Context, limit, offset int) ([]*domain.Wallet, error)
	CountByTenant(ctx context.Context, tenantID string) (int, error)
	UpdateSyncCursor(ctx context.Context, walletID, cursor string) error
	UpsertBalance(ctx context.Context, walletID, assetCode, issuer string, balance decimal.Decimal) error
	GetBalances(ctx context.Context, walletID string) ([]domain.BalanceRecord, error)
}
