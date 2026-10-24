package wallet

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
)

// SweepRepository loads what the sweep engine scans and records what it moved.
type SweepRepository interface {
	// ListSweepTargets returns the custody settings of every tenant and mode
	// that has opted in to automated sweeping.
	ListSweepTargets(ctx context.Context) ([]*domain.TenantCustodySettings, error)
	// ListCustodialWallets pages a tenant's custodial wallets in ID order,
	// starting after afterID (empty for the first page). Wallets with a
	// pending or completed account closure are excluded.
	ListCustodialWallets(ctx context.Context, tenantID string, mode domain.Mode, afterID string, limit int) ([]*domain.Wallet, error)
	CountPendingTransfers(ctx context.Context, walletID string) (int, error)
	RecordSweep(ctx context.Context, sweep *domain.CustodialSweep) error
}

type sweepRepository struct {
	db ClosureDB
}

// NewSweepRepository creates a Postgres-backed sweep repository.
func NewSweepRepository(db ClosureDB) SweepRepository {
	return &sweepRepository{db: db}
}

func (r *sweepRepository) ListSweepTargets(ctx context.Context) ([]*domain.TenantCustodySettings, error) {
	rows, err := r.db.Query(ctx, `SELECT `+custodySettingsColumns+`
		FROM tenant_custody_settings
		WHERE auto_sweep_enabled
		ORDER BY tenant_id, mode
	`)
	if err != nil {
		return nil, fmt.Errorf("list sweep targets: %w", err)
	}
	defer rows.Close()

	targets := []*domain.TenantCustodySettings{}
	for rows.Next() {
		s := &domain.TenantCustodySettings{}
		if err := rows.Scan(custodySettingsDest(s)...); err != nil {
			return nil, fmt.Errorf("scan sweep target: %w", err)
		}
		targets = append(targets, s)
	}
	return targets, rows.Err()
}

func (r *sweepRepository) ListCustodialWallets(ctx context.Context, tenantID string, mode domain.Mode, afterID string, limit int) ([]*domain.Wallet, error) {
	if afterID == "" {
		afterID = uuid.Nil.String()
	}
	rows, err := r.db.Query(ctx, `
		SELECT w.id, w.public_key, w.encrypted_secret, w.tenant_id, w.mode, w.custody_type
		FROM wallets w
		WHERE w.tenant_id = $1
		  AND w.mode = $2
		  AND w.custody_type = $3
		  AND w.id > $4::uuid
		  AND NOT EXISTS (
		      SELECT 1 FROM account_closures c
		      WHERE c.wallet_id = w.id AND c.status IN ('pending', 'completed')
		  )
		ORDER BY w.id
		LIMIT $5
	`, tenantID, mode, domain.CustodyCustodial, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list custodial wallets: %w", err)
	}
	defer rows.Close()

	wallets := []*domain.Wallet{}
	for rows.Next() {
		w := &domain.Wallet{}
		if err := rows.Scan(&w.ID, &w.PublicKey, &w.EncryptedSecret, &w.TenantID, &w.Mode, &w.CustodyType); err != nil {
			return nil, fmt.Errorf("scan custodial wallet: %w", err)
		}
		wallets = append(wallets, w)
	}
	return wallets, rows.Err()
}

func (r *sweepRepository) CountPendingTransfers(ctx context.Context, walletID string) (int, error) {
	return countPendingTransfers(ctx, r.db, walletID)
}

func (r *sweepRepository) RecordSweep(ctx context.Context, sweep *domain.CustodialSweep) error {
	if sweep.ID == "" {
		sweep.ID = uuid.New().String()
	}
	items, err := json.Marshal(sweep.Items)
	if err != nil {
		return fmt.Errorf("encode sweep items: %w", err)
	}
	err = r.db.QueryRow(ctx, `
		INSERT INTO custodial_sweeps
		(id, tenant_id, mode, destination_address, transaction_hash, status, fee_stroops, items, error_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at
	`, sweep.ID, sweep.TenantID, sweep.Mode, sweep.DestinationAddress, sweep.TransactionHash,
		sweep.Status, sweep.FeeStroops, items, sweep.ErrorMessage).Scan(&sweep.CreatedAt)
	if err != nil {
		return fmt.Errorf("record custodial sweep: %w", err)
	}
	return nil
}
