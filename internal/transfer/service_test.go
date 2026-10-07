package transfer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fees"
	"github.com/Lumen-Nexora/Nexora/internal/queue"
	"github.com/Lumen-Nexora/Nexora/internal/server/idempotency"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/Lumen-Nexora/Nexora/internal/transfer"
	"github.com/Lumen-Nexora/Nexora/internal/wallet"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
	"time"
)

type basicMockWalletRepo struct {
	wallets map[string]*domain.Wallet
}

func (m *basicMockWalletRepo) Create(ctx context.Context, w *domain.Wallet) error { return nil }
func (m *basicMockWalletRepo) GetByID(ctx context.Context, id string) (*domain.Wallet, error) {
	w, ok := m.wallets[id]
	if !ok {
		return nil, domain.ErrWalletNotFound
	}
	return w, nil
}

func (m *basicMockWalletRepo) GetByPublicKey(ctx context.Context, pubKey string) (*domain.Wallet, error) {
	return nil, nil
}

func (m *basicMockWalletRepo) List(ctx context.Context, limit, offset int) ([]*domain.Wallet, error) {
	return nil, nil
}

func (m *basicMockWalletRepo) CountByTenant(ctx context.Context, tenantID string) (int, error) {
	return 0, nil
}

func (m *basicMockWalletRepo) UpsertBalance(ctx context.Context, walletID, assetCode, issuer string, balance decimal.Decimal) error {
	return nil
}

func (m *basicMockWalletRepo) GetBalances(ctx context.Context, walletID string) ([]domain.BalanceRecord, error) {
	return nil, nil
}

func (m *basicMockWalletRepo) UpdateSyncCursor(ctx context.Context, walletID, cursor string) error {
	return nil
}

type basicMockTxRepo struct {
	txs []*domain.Transaction
}

func (m *basicMockTxRepo) Create(ctx context.Context, tx *domain.Transaction) error {
	m.txs = append(m.txs, tx)
	return nil
}
func (m *basicMockTxRepo) GetByID(ctx context.Context, id string) (*domain.Transaction, error) {
	return nil, nil
}

func (m *basicMockTxRepo) CreateWithMonthlyLimit(ctx context.Context, tx *domain.Transaction, tenantID string, year int, month time.Month, limit int) error {
	return nil
}

func (m *basicMockTxRepo) ClaimForSubmission(ctx context.Context, id string) error {
	return nil
}

func (m *basicMockTxRepo) UpdateStatus(ctx context.Context, id string, status domain.TransactionStatus, txHash string) error {
	return nil
}

func (m *basicMockTxRepo) UpsertByTxHash(ctx context.Context, tx *domain.Transaction) error {
	return nil
}

func (m *basicMockTxRepo) ListByWallet(ctx context.Context, walletID string, limit, offset int) ([]*domain.Transaction, error) {
	return m.txs, nil
}

func (m *basicMockTxRepo) ExistsByTxHash(ctx context.Context, txHash string) (bool, error) {
	return false, nil
}

func (m *basicMockTxRepo) GetByIdempotencyKey(ctx context.Context, orgID, idempotencyKey string) (*domain.Transaction, error) {
	return nil, domain.ErrTransactionNotFound
}

func (m *basicMockTxRepo) ListByBatch(ctx context.Context, batchID string) ([]*domain.Transaction, error) {
	return nil, nil
}

func (m *basicMockTxRepo) CountMonthlyTransfersByTenant(ctx context.Context, tenantID string, year int, month time.Month) (int, error) {
	return 0, nil
}

type basicMockFeeSvc struct{}

func (m *basicMockFeeSvc) GetSchedule(ctx context.Context, orgID string) (*domain.FeeSchedule, error) {
	return nil, nil
}
func (m *basicMockFeeSvc) CalculateTransferFee(ctx context.Context, orgID, asset string, amount decimal.Decimal) (*fees.TransferFee, error) {
	return &fees.TransferFee{
		FeeAmount: decimal.Zero,
		NetAmount: amount,
		FeeBps:    0,
	}, nil
}

func (m *basicMockFeeSvc) CalculateConversionFee(ctx context.Context, orgID, asset string, amount decimal.Decimal) (*fees.TransferFee, error) {
	return nil, nil
}

func (m *basicMockFeeSvc) RecordCollection(ctx context.Context, collection *domain.FeeCollection) error {
	return nil
}
func (m *basicMockFeeSvc) SetSchedule(_ context.Context, _ *domain.FeeSchedule) error {
	return nil
}
func (m *basicMockFeeSvc) ListCollected(_ context.Context, _, _ *time.Time, _ *string, _, _ int) ([]*domain.FeeCollection, error) {
	return nil, nil
}
func (m *basicMockFeeSvc) ListCollectedSummary(ctx context.Context, orgID string, since *time.Time) ([]domain.FeeCollectionSummary, error) {
	return nil, nil
}

func TestInitiateTransfer_Success(t *testing.T) {
	wr := &basicMockWalletRepo{
		wallets: map[string]*domain.Wallet{
			"w1": {ID: "w1", PublicKey: "G1"},
			"w2": {ID: "w2", PublicKey: "G2"},
		},
	}
	tr := &basicMockTxRepo{}
	feeSvc := &basicMockFeeSvc{}

	svc := transfer.NewService(tr, wr, feeSvc, nil)

	// Since XLM doesn't require trustline validation, it should succeed
	tx, err := svc.InitiateTransfer(context.Background(), "w1", "w2", "XLM", decimal.NewFromInt(10))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tx.Asset != "XLM" || tx.Amount.Cmp(decimal.NewFromInt(10)) != 0 {
		t.Errorf("unexpected tx values")
	}

	if len(tr.txs) != 1 {
		t.Errorf("expected transaction to be saved")
	}
}

func TestInitiateTransfer_SelfTransfer(t *testing.T) {
	wr := &basicMockWalletRepo{}
	tr := &basicMockTxRepo{}
	feeSvc := &basicMockFeeSvc{}

	svc := transfer.NewService(tr, wr, feeSvc, nil)

	_, err := svc.InitiateTransfer(context.Background(), "w1", "w1", "XLM", decimal.NewFromInt(10))
	if !errors.Is(err, domain.ErrSelfTransfer) {
		t.Errorf("expected ErrSelfTransfer, got %v", err)
	}
}

// recordMockTxRepo additionally implements transfer.IdempotencyRecordRepository
// so the service can reconcile a retried request against the transfer created
// by the original (now dead) process.
type recordMockTxRepo struct {
	basicMockTxRepo
	byRecord map[string]*domain.Transaction
	creates  int
}

func (m *recordMockTxRepo) Create(ctx context.Context, tx *domain.Transaction) error {
	m.creates++
	return m.basicMockTxRepo.Create(ctx, tx)
}

func (m *recordMockTxRepo) GetByIdempotencyRecordID(_ context.Context, recordID string) (*domain.Transaction, error) {
	if tx, ok := m.byRecord[recordID]; ok {
		return tx, nil
	}
	return nil, domain.ErrTransactionNotFound
}

// recordingQueue counts the settlement tasks enqueued for a transaction.
type recordingQueue struct {
	enqueued []string
}

func (q *recordingQueue) EnqueueTransfer(_ context.Context, transactionID string) error {
	q.enqueued = append(q.enqueued, transactionID)
	return nil
}

// TestInitiateTransferIdempotent_RecoversOriginalAfterCrash simulates a process
// that persisted the original transfer and linked it to an idempotency record,
// then crashed before the HTTP response was recorded. The retry must return the
// original transfer without persisting or enqueueing a second one.
func TestInitiateTransferIdempotent_RecoversOriginalAfterCrash(t *testing.T) {
	original := &domain.Transaction{
		ID:         "tx-original",
		FromWallet: "w1",
		ToWallet:   "w2",
		Asset:      "XLM",
		Amount:     decimal.NewFromInt(10),
		Status:     domain.StatusPending,
	}
	repo := &recordMockTxRepo{byRecord: map[string]*domain.Transaction{"record-1": original}}
	wr := &basicMockWalletRepo{
		wallets: map[string]*domain.Wallet{
			"w1": {ID: "w1", PublicKey: "G1"},
			"w2": {ID: "w2", PublicKey: "G2"},
		},
	}
	q := &recordingQueue{}
	svc := transfer.NewService(repo, wr, &basicMockFeeSvc{}, q)

	ctx := tenant.WithID(context.Background(), "org-1")
	ctx = tenant.WithMode(ctx, domain.ModeLive)
	ctx = idempotency.WithRecordID(ctx, "record-1")

	tx, err := svc.InitiateTransferIdempotent(ctx, "w1", "w2", "XLM", decimal.NewFromInt(10), uuid.NewString())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx == nil || tx.ID != original.ID {
		t.Fatalf("expected the original transfer %q, got %+v", original.ID, tx)
	}
	if repo.creates != 0 {
		t.Errorf("recovery must not create another transfer, got %d creates", repo.creates)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("recovery must not enqueue another settlement, got %v", q.enqueued)
	}
}

func TestListTransactions(t *testing.T) {
	wr := &basicMockWalletRepo{}
	tr := &basicMockTxRepo{
		txs: []*domain.Transaction{
			{ID: "tx1", FromWallet: "w1"},
			{ID: "tx2", FromWallet: "w1"},
		},
	}
	feeSvc := &basicMockFeeSvc{}

	svc := transfer.NewService(tr, wr, feeSvc, nil)

	txs, err := svc.ListTransactions(context.Background(), "w1", 10, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(txs) != 2 {
		t.Errorf("expected 2 txs, got %d", len(txs))
	}
}

type failingQueueRepo struct {
	transfer.Repository
}

func (r *failingQueueRepo) Create(ctx context.Context, tx *domain.Transaction) error {
	return nil
}

type failingWalletRepo struct {
	wallet.Repository
}

func (r *failingWalletRepo) GetByID(ctx context.Context, id string) (*domain.Wallet, error) {
	return &domain.Wallet{ID: id, PublicKey: "GXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}, nil
}

func (r *failingWalletRepo) GetBalances(context.Context, string) ([]domain.BalanceRecord, error) {
	return []domain.BalanceRecord{{AssetCode: "USDC", Balance: "100"}}, nil
}

type failingFeeService struct {
	fees.Service
}

func (s *failingFeeService) CalculateTransferFee(ctx context.Context, tenantID, asset string, amount decimal.Decimal) (*fees.TransferFee, error) {
	return &fees.TransferFee{FeeAmount: decimal.Zero, NetAmount: amount, FeeBps: 0}, nil
}

func (s *failingFeeService) CalculateConversionFee(ctx context.Context, tenantID, asset string, amount decimal.Decimal) (*fees.TransferFee, error) {
	return nil, nil
}

func (s *failingFeeService) RecordCollection(ctx context.Context, collection *domain.FeeCollection) error {
	return nil
}

func (s *failingFeeService) ListCollectedSummary(ctx context.Context, tenantID string, since *time.Time) ([]domain.FeeCollectionSummary, error) {
	return nil, nil
}

func (s *failingFeeService) GetSchedule(ctx context.Context, tenantID string) (*domain.FeeSchedule, error) {
	return nil, nil
}

func TestInitiateTransfer_EnqueueFailure(t *testing.T) {
	// If queue client is provided with a bad address or closed, or if we test service behavior with a queue that fails.
}

func TestServiceEnqueueFailureObservable(t *testing.T) {
	q := queue.NewClientWithOptions(asynq.RedisClientOpt{Addr: "127.0.0.1:1"})
	svc := transfer.NewService(&failingQueueRepo{}, &failingWalletRepo{}, &failingFeeService{}, q)
	_, err := svc.InitiateTransfer(context.Background(), "wallet-1", "wallet-2", "USDC", decimal.NewFromInt(10))
	if err == nil {
		t.Errorf("expected error on failed enqueue, got nil")
	}
}
