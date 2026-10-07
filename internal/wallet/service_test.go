package wallet

import (
	"context"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/shopspring/decimal"
)

type balanceTestRepository struct {
	wallet *domain.Wallet
}

func (r *balanceTestRepository) Create(context.Context, *domain.Wallet) error { return nil }
func (r *balanceTestRepository) GetByID(context.Context, string) (*domain.Wallet, error) {
	return r.wallet, nil
}
func (r *balanceTestRepository) GetByPublicKey(context.Context, string) (*domain.Wallet, error) {
	return r.wallet, nil
}
func (r *balanceTestRepository) List(context.Context, int, int) ([]*domain.Wallet, error) {
	return []*domain.Wallet{r.wallet}, nil
}
func (r *balanceTestRepository) CountByTenant(context.Context, string) (int, error) { return 0, nil }
func (r *balanceTestRepository) UpsertBalance(context.Context, string, string, string, decimal.Decimal) error {
	return nil
}
func (r *balanceTestRepository) GetBalances(context.Context, string) ([]domain.BalanceRecord, error) {
	return []domain.BalanceRecord{}, nil
}
func (r *balanceTestRepository) UpdateSyncCursor(context.Context, string, string) error { return nil }

func TestServiceUsesAuthenticatedEnvironmentForWalletReads(t *testing.T) {
	repo := &balanceTestRepository{wallet: &domain.Wallet{ID: "wallet-1", Mode: domain.ModeTest}}
	svc := NewService(repo, nil, nil)
	ctx := tenant.WithID(context.Background(), "tenant-1")
	ctx = tenant.WithMode(ctx, domain.ModeTest)

	got, err := svc.GetWalletForHandler(ctx, "wallet-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != domain.ModeTest {
		t.Fatalf("wallet mode = %q, want test", got.Mode)
	}
}
