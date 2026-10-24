package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// ClosureDB is the subset of postgres.DB the closure repository uses. It is
// declared here because wallet cannot import postgres without an import cycle
// (postgres -> paymentlink -> fiat -> fx -> wallet).
type ClosureDB interface {
	Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
	Query(context.Context, string, ...interface{}) (pgx.Rows, error)
	QueryRow(context.Context, string, ...interface{}) pgx.Row
}

// ClosureRepository persists account closures, the tenant custody settings
// they depend on, and answers the pending-transfer safety check.
type ClosureRepository interface {
	// Create inserts a pending closure. It returns ErrClosureAlreadyExists when
	// the wallet already has a pending or completed closure.
	Create(ctx context.Context, closure *domain.AccountClosure) error
	Get(ctx context.Context, id, tenantID string) (*domain.AccountClosure, error)
	List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.AccountClosure, error)
	// Complete and Fail move a pending closure to its final state. The
	// account_closures trigger rejects any later change.
	Complete(ctx context.Context, id, txHash string) error
	Fail(ctx context.Context, id string, reason error) error

	// CountPendingTransfers counts non-terminal transfers into or out of the
	// wallet.
	CountPendingTransfers(ctx context.Context, walletID string) (int, error)

	GetCustodySettings(ctx context.Context, tenantID string, mode domain.Mode) (*domain.TenantCustodySettings, error)
	UpsertCustodySettings(ctx context.Context, settings *domain.TenantCustodySettings) error
}

// pendingTransferStatuses are the transaction states that may still move
// funds through a wallet. Closing the account under any of them would strand
// the transfer.
var pendingTransferStatuses = []string{
	string(domain.StatusPending),
	string(domain.StatusSubmitted),
	string(domain.StatusComplianceHold),
	string(domain.StatusApprovalPending),
}

type closureRepository struct {
	db ClosureDB
}

// NewClosureRepository creates a new account closure repository.
func NewClosureRepository(db ClosureDB) ClosureRepository {
	return &closureRepository{db: db}
}

const closureColumns = `id, tenant_id, mode, wallet_id, destination_wallet, COALESCE(destination_address, ''),
	actor_type, actor_id, justification, removed_trustlines, native_balance,
	transaction_hash, status, error_message, created_at, completed_at`

func (r *closureRepository) Create(ctx context.Context, closure *domain.AccountClosure) error {
	closure.ID = uuid.New().String()
	closure.CreatedAt = time.Now().UTC()
	closure.Status = domain.ClosureStatusPending
	if closure.RemovedTrustlines == nil {
		closure.RemovedTrustlines = []domain.ClosedTrustline{}
	}

	trustlines, err := json.Marshal(closure.RemovedTrustlines)
	if err != nil {
		return fmt.Errorf("encode removed trustlines: %w", err)
	}

	_, err = r.db.Exec(ctx, `
		INSERT INTO account_closures
		(id, tenant_id, mode, wallet_id, destination_wallet, destination_address,
		 actor_type, actor_id, justification, removed_trustlines, native_balance,
		 status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, closure.ID, closure.TenantID, closure.Mode, closure.WalletID, closure.DestinationWallet,
		closure.DestinationAddress, closure.ActorType, closure.ActorID, closure.Justification,
		trustlines, closure.NativeBalance, closure.Status, closure.CreatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrClosureAlreadyExists
	}
	if err != nil {
		return fmt.Errorf("insert account closure: %w", err)
	}
	return nil
}

func (r *closureRepository) Get(ctx context.Context, id, tenantID string) (*domain.AccountClosure, error) {
	row := r.db.QueryRow(ctx, `SELECT `+closureColumns+`
		FROM account_closures
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID)
	closure, err := scanClosure(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAccountClosureNotFound
	}
	return closure, err
}

func (r *closureRepository) List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.AccountClosure, error) {
	rows, err := r.db.Query(ctx, `SELECT `+closureColumns+`
		FROM account_closures
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list account closures: %w", err)
	}
	defer rows.Close()

	closures := []*domain.AccountClosure{}
	for rows.Next() {
		closure, err := scanClosure(rows)
		if err != nil {
			return nil, err
		}
		closures = append(closures, closure)
	}
	return closures, rows.Err()
}

func (r *closureRepository) Complete(ctx context.Context, id, txHash string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE account_closures
		SET status = $1, transaction_hash = $2, completed_at = $3
		WHERE id = $4 AND status = $5
	`, domain.ClosureStatusCompleted, txHash, time.Now().UTC(), id, domain.ClosureStatusPending)
	if err != nil {
		return fmt.Errorf("complete account closure: %w", err)
	}
	return nil
}

func (r *closureRepository) Fail(ctx context.Context, id string, reason error) error {
	msg := "unknown error"
	if reason != nil {
		msg = reason.Error()
	}
	_, err := r.db.Exec(ctx, `
		UPDATE account_closures
		SET status = $1, error_message = $2, completed_at = $3
		WHERE id = $4 AND status = $5
	`, domain.ClosureStatusFailed, msg, time.Now().UTC(), id, domain.ClosureStatusPending)
	if err != nil {
		return fmt.Errorf("fail account closure: %w", err)
	}
	return nil
}

func (r *closureRepository) CountPendingTransfers(ctx context.Context, walletID string) (int, error) {
	return countPendingTransfers(ctx, r.db, walletID)
}

// countPendingTransfers counts non-terminal transfers into or out of a wallet.
// Closure and sweep both refuse to touch a wallet with funds in flight.
func countPendingTransfers(ctx context.Context, db ClosureDB, walletID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM transactions
		WHERE (from_wallet = $1 OR to_wallet = $1)
		  AND status::text = ANY($2)
	`, walletID, pendingTransferStatuses).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending transfers: %w", err)
	}
	return n, nil
}

func (r *closureRepository) GetCustodySettings(ctx context.Context, tenantID string, mode domain.Mode) (*domain.TenantCustodySettings, error) {
	s := &domain.TenantCustodySettings{}
	err := r.db.QueryRow(ctx, `
		SELECT `+custodySettingsColumns+`
		FROM tenant_custody_settings
		WHERE tenant_id = $1 AND mode = $2
	`, tenantID, mode).Scan(custodySettingsDest(s)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCustodySettingsNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get custody settings: %w", err)
	}
	return s, nil
}

func (r *closureRepository) UpsertCustodySettings(ctx context.Context, s *domain.TenantCustodySettings) error {
	err := r.db.QueryRow(ctx, `
		INSERT INTO tenant_custody_settings
		(tenant_id, mode, master_address, primary_settlement_wallet_id, auto_sweep_enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, mode) DO UPDATE
		SET master_address = EXCLUDED.master_address,
		    primary_settlement_wallet_id = EXCLUDED.primary_settlement_wallet_id,
		    auto_sweep_enabled = EXCLUDED.auto_sweep_enabled,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = NOW()
		RETURNING created_at, updated_at
	`, s.TenantID, s.Mode, s.MasterAddress, s.PrimarySettlementWalletID, s.AutoSweepEnabled, s.UpdatedBy).
		Scan(&s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert custody settings: %w", err)
	}
	return nil
}

const custodySettingsColumns = `tenant_id, mode, master_address, primary_settlement_wallet_id,
	auto_sweep_enabled, updated_by, created_at, updated_at`

func custodySettingsDest(s *domain.TenantCustodySettings) []interface{} {
	return []interface{}{&s.TenantID, &s.Mode, &s.MasterAddress, &s.PrimarySettlementWalletID,
		&s.AutoSweepEnabled, &s.UpdatedBy, &s.CreatedAt, &s.UpdatedAt}
}

func scanClosure(row pgx.Row) (*domain.AccountClosure, error) {
	c := &domain.AccountClosure{}
	var trustlines []byte
	var native *decimal.Decimal
	err := row.Scan(
		&c.ID, &c.TenantID, &c.Mode, &c.WalletID, &c.DestinationWallet, &c.DestinationAddress,
		&c.ActorType, &c.ActorID, &c.Justification, &trustlines, &native,
		&c.TransactionHash, &c.Status, &c.ErrorMessage, &c.CreatedAt, &c.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	c.NativeBalance = native
	c.RemovedTrustlines = []domain.ClosedTrustline{}
	if len(trustlines) > 0 {
		if err := json.Unmarshal(trustlines, &c.RemovedTrustlines); err != nil {
			return nil, fmt.Errorf("decode removed trustlines: %w", err)
		}
	}
	return c, nil
}
