package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stellar/go/xdr"
)

var ErrNotFound = errors.New("multisig record not found")

// MultisigRepo persists multisig configurations and pending signature state.
type MultisigRepo struct {
	db *sql.DB
}

// NewMultisigRepo returns a repository backed by the given database.
func NewMultisigRepo(db *sql.DB) *MultisigRepo {
	return &MultisigRepo{db: db}
}

// SaveConfig upserts a tenant's multisig configuration for an account.
func (r *MultisigRepo) SaveConfig(ctx context.Context, tenantID, accountID string, cfg any) error {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO multisig_configs (tenant_id, account_id, config, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (tenant_id, account_id)
		DO UPDATE SET config = EXCLUDED.config, updated_at = NOW()`,
		tenantID, accountID, payload)
	return err
}

// LoadConfig retrieves a tenant's multisig configuration into dst.
func (r *MultisigRepo) LoadConfig(ctx context.Context, tenantID, accountID string, dst any) error {
	var payload []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT config FROM multisig_configs WHERE tenant_id = $1 AND account_id = $2`,
		tenantID, accountID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, dst)
}

// SavePendingSignature records a collected co-signer signature for a pending
// transaction. Duplicate (tx_hash, signer) pairs are ignored.
func (r *MultisigRepo) SavePendingSignature(ctx context.Context, tenantID, txHash, signer string, sig xdr.DecoratedSignature) error {
	raw, err := sig.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode signature: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO multisig_signatures (tenant_id, tx_hash, signer, signature, created_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (tenant_id, tx_hash, signer) DO NOTHING`,
		tenantID, txHash, signer, raw)
	return err
}

// LoadPendingSignatures returns all stored signatures for a transaction,
// keyed by signer address.
func (r *MultisigRepo) LoadPendingSignatures(ctx context.Context, tenantID, txHash string) (map[string]xdr.DecoratedSignature, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT signer, signature FROM multisig_signatures
		WHERE tenant_id = $1 AND tx_hash = $2`,
		tenantID, txHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]xdr.DecoratedSignature)
	for rows.Next() {
		var signer string
		var raw []byte
		if err := rows.Scan(&signer, &raw); err != nil {
			return nil, err
		}
		var sig xdr.DecoratedSignature
		if err := sig.UnmarshalBinary(raw); err != nil {
			return nil, fmt.Errorf("decode signature for %s: %w", signer, err)
		}
		out[signer] = sig
	}
	return out, rows.Err()
}

// MarkSubmitted records that a transaction has been submitted to Horizon.
func (r *MultisigRepo) MarkSubmitted(ctx context.Context, tenantID, txHash string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE multisig_signatures SET submitted_at = NOW()
		WHERE tenant_id = $1 AND tx_hash = $2 AND submitted_at IS NULL`,
		tenantID, txHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
