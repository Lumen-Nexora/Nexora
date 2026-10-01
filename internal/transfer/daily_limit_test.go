package transfer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxa/fluxa/internal/domain"
	"github.com/fluxa/fluxa/internal/fees"
	"github.com/fluxa/fluxa/internal/tenant"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Mock implementations for daily limit tests
// ---------------------------------------------------------------------------

type dailyLimitMockTxRepo struct {
	count atomic.Int64
	limit int
}

func (m *dailyLimitMockTxRepo) Create(_ context.Context, tx *domain.Transaction) error {
	return nil
}

func (m *dailyLimitMockTxRepo) CreateWithDailyLimit(_ context.Context, tx *domain.Transaction, _ string, _ time.Time, limit int) error {
	// Simulate atomic check-and-increment.
	for {
		current := m.count.Load()
		if current >= int64(limit) {
			return domain.ErrDailyTransferLimitReached
		}
		if m.count.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

func (m *dailyLimitMockTxRepo) GetByID(_ context.Context, id string) (*domain.Transaction, error) {
	return nil, domain.ErrTransactionNotFound
}
func (m *dailyLimitMockTxRepo) ClaimForSubmission(_ context.Context, _ string) error {
	return nil
}
func (m *dailyLimitMockTxRepo) UpdateStatus(_ context.Context, _ string, _ domain.TransactionStatus, _ string) error {
	return nil
}
func (m *dailyLimitMockTxRepo) ListByWallet(_ context.Context, _ string, _, _ int) ([]*domain.Transaction, error) {
	return nil, nil
}
func (m *dailyLimitMockTxRepo) ListByBatch(_ context.Context, _ string) ([]*domain.Transaction, error) {
	return nil, nil
}
func (m *dailyLimitMockTxRepo) CountMonthlyTransfersByTenant(_ context.Context, _ string, _ int, _ time.Month) (int, error) {
	return int(m.count.Load()), nil
}
func (m *dailyLimitMockTxRepo) CountDailyTransfersByTenant(_ context.Context, _ string, _ time.Time) (int, error) {
	return int(m.count.Load()), nil
}
func (m *dailyLimitMockTxRepo) UpsertByTxHash(_ context.Context, _ *domain.Transaction) error {
	return nil
}
func (m *dailyLimitMockTxRepo) ExistsByTxHash(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (m *dailyLimitMockTxRepo) GetByIdempotencyKey(_ context.Context, _, _ string) (*domain.Transaction, error) {
	return nil, domain.ErrTransactionNotFound
}
func (m *dailyLimitMockTxRepo) CreateWithMonthlyLimit(_ context.Context, tx *domain.Transaction, _ string, _ int, _ time.Month, limit int) error {
	return nil
}

type dailyLimitMockWalletRepo struct{}

func (m *dailyLimitMockWalletRepo) Create(_ context.Context, _ *domain.Wallet) error { return nil }
func (m *dailyLimitMockWalletRepo) GetByID(_ context.Context, id string) (*domain.Wallet, error) {
	return &domain.Wallet{ID: id, PublicKey: "G" + id}, nil
}
func (m *dailyLimitMockWalletRepo) GetByPublicKey(_ context.Context, _ string) (*domain.Wallet, error) {
	return nil, domain.ErrWalletNotFound
}
func (m *dailyLimitMockWalletRepo) List(_ context.Context, _, _ int) ([]*domain.Wallet, error) {
	return nil, nil
}
func (m *dailyLimitMockWalletRepo) CountByTenant(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (m *dailyLimitMockWalletRepo) UpsertBalance(_ context.Context, _, _, _ string, _ decimal.Decimal) error {
	return nil
}
func (m *dailyLimitMockWalletRepo) GetBalances(_ context.Context, _ string) ([]domain.BalanceRecord, error) {
	return nil, nil
}
func (m *dailyLimitMockWalletRepo) UpdateSyncCursor(_ context.Context, _, _ string) error { return nil }

type dailyLimitMockTenantRepo struct {
	tenant *domain.Tenant
}

func (m *dailyLimitMockTenantRepo) GetByID(_ context.Context, id string) (*domain.Tenant, error) {
	return m.tenant, nil
}

type dailyLimitMockFeeSvc struct{}

func (m *dailyLimitMockFeeSvc) SetSchedule(_ context.Context, _ *domain.FeeSchedule) error {
	return nil
}
func (m *dailyLimitMockFeeSvc) GetSchedule(_ context.Context, _ string) (*domain.FeeSchedule, error) {
	return nil, nil
}
func (m *dailyLimitMockFeeSvc) CalculateTransferFee(_ context.Context, _, _ string, _ decimal.Decimal) (*fees.TransferFee, error) {
	return &fees.TransferFee{FeeAmount: decimal.Zero, NetAmount: decimal.NewFromInt(10), FeeBps: 0}, nil
}
func (m *dailyLimitMockFeeSvc) CalculateConversionFee(_ context.Context, _, _ string, _ decimal.Decimal) (*fees.TransferFee, error) {
	return &fees.TransferFee{FeeAmount: decimal.Zero, NetAmount: decimal.NewFromInt(10), FeeBps: 0}, nil
}
func (m *dailyLimitMockFeeSvc) RecordCollection(_ context.Context, _ *domain.FeeCollection) error {
	return nil
}
func (m *dailyLimitMockFeeSvc) ListCollected(_ context.Context, _, _ *time.Time, _ *string, _, _ int) ([]*domain.FeeCollection, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestTransferWithinDailyLimit(t *testing.T) {
	limit := 3
	txRepo := &dailyLimitMockTxRepo{limit: limit}
	wRepo := &dailyLimitMockWalletRepo{}
	feeSvc := &dailyLimitMockFeeSvc{}
	tenantRepo := &dailyLimitMockTenantRepo{
		tenant: &domain.Tenant{ID: "t-1", AccountType: domain.AccountTypeIndividual, MaxTransfersPerDay: &limit},
	}

	svc := NewService(txRepo, wRepo, feeSvc, nil, tenantRepo)

	ctx := tenant.WithID(context.Background(), "t-1")

	for i := 0; i < 3; i++ {
		tx, err := svc.InitiateTransfer(ctx, "w-from", "w-to", "XLM", decimal.NewFromInt(1))
		if err != nil {
			t.Fatalf("transfer %d: unexpected error: %v", i, err)
		}
		if tx == nil {
			t.Fatalf("transfer %d: expected transaction", i)
		}
	}
}

func TestTransferAtExactDailyLimit(t *testing.T) {
	limit := 2
	txRepo := &dailyLimitMockTxRepo{limit: limit}
	wRepo := &dailyLimitMockWalletRepo{}
	feeSvc := &dailyLimitMockFeeSvc{}
	tenantRepo := &dailyLimitMockTenantRepo{
		tenant: &domain.Tenant{ID: "t-1", AccountType: domain.AccountTypeIndividual, MaxTransfersPerDay: &limit},
	}

	svc := NewService(txRepo, wRepo, feeSvc, nil, tenantRepo)

	ctx := tenant.WithID(context.Background(), "t-1")

	// First two should succeed (at limit).
	for i := 0; i < 2; i++ {
		_, err := svc.InitiateTransfer(ctx, "w-from", "w-to", "XLM", decimal.NewFromInt(1))
		if err != nil {
			t.Fatalf("transfer %d: unexpected error: %v", i, err)
		}
	}

	// Third should fail (exceeds limit).
	_, err := svc.InitiateTransfer(ctx, "w-from", "w-to", "XLM", decimal.NewFromInt(1))
	if err == nil {
		t.Fatal("expected ErrDailyTransferLimitReached at exact limit, got nil")
	}
	if err != domain.ErrDailyTransferLimitReached {
		t.Fatalf("expected ErrDailyTransferLimitReached, got %v", err)
	}
}

func TestTransferNoDailyLimit(t *testing.T) {
	txRepo := &dailyLimitMockTxRepo{limit: 0}
	wRepo := &dailyLimitMockWalletRepo{}
	feeSvc := &dailyLimitMockFeeSvc{}
	tenantRepo := &dailyLimitMockTenantRepo{
		tenant: &domain.Tenant{ID: "t-1", AccountType: domain.AccountTypeIndividual, MaxTransfersPerDay: nil},
	}

	svc := NewService(txRepo, wRepo, feeSvc, nil, tenantRepo)

	ctx := tenant.WithID(context.Background(), "t-1")

	// Should succeed even with multiple transfers when no limit is set.
	for i := 0; i < 10; i++ {
		_, err := svc.InitiateTransfer(ctx, "w-from", "w-to", "XLM", decimal.NewFromInt(1))
		if err != nil {
			t.Fatalf("transfer %d: unexpected error: %v", i, err)
		}
	}
}
