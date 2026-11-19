package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// MemoPolicyRepo persists the per-wallet inbound memo rules and the deposits
// that could not be mapped to a customer.
type MemoPolicyRepo struct {
	db DB
}

func NewMemoPolicyRepo(db DB) *MemoPolicyRepo {
	return &MemoPolicyRepo{db: db}
}

// GetMemoPolicy returns the policy of a wallet, or nil when the wallet has
// none. The lookup is by wallet only: the wallet ID is globally unique and
// the tenant that owns the policy is stored on the row itself.
//
// The read takes a share lock so it runs on the primary: the indexer must
// not credit a deposit that a policy written a moment ago was supposed to
// hold back, and a replica that is a few hundred milliseconds behind would
// hand it out.
func (r *MemoPolicyRepo) GetMemoPolicy(ctx context.Context, walletID string) (*domain.MemoPolicy, error) {
	var p domain.MemoPolicy
	var seconds float64
	err := r.db.QueryRow(ctx,
		`SELECT id::text, tenant_id::text, wallet_id::text, format, pattern, require_memo, auto_refund, EXTRACT(EPOCH FROM refund_after), enabled, created_at, updated_at
		 FROM wallet_memo_policies WHERE wallet_id = $1 FOR SHARE`,
		walletID,
	).Scan(&p.ID, &p.TenantID, &p.WalletID, &p.Format, &p.Pattern, &p.RequireMemo, &p.AutoRefund, &seconds, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get memo policy for wallet %s: %w", walletID, err)
	}
	p.RefundAfter = time.Duration(seconds * float64(time.Second))
	return &p, nil
}

// PutMemoPolicy creates or replaces the policy of a wallet.
func (r *MemoPolicyRepo) PutMemoPolicy(ctx context.Context, policy *domain.MemoPolicy) (*domain.MemoPolicy, error) {
	if policy.ID == "" {
		policy.ID = newUUID()
	}
	now := time.Now().UTC()
	policy.CreatedAt = now
	policy.UpdatedAt = now

	var stored domain.MemoPolicy
	var seconds float64
	err := r.db.QueryRow(ctx,
		`INSERT INTO wallet_memo_policies (id, tenant_id, wallet_id, format, pattern, require_memo, auto_refund, refund_after, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8 * INTERVAL '1 second', $9, $10, $11)
		 ON CONFLICT (tenant_id, wallet_id) DO UPDATE
		 SET format = EXCLUDED.format, pattern = EXCLUDED.pattern, require_memo = EXCLUDED.require_memo,
		     auto_refund = EXCLUDED.auto_refund, refund_after = EXCLUDED.refund_after, enabled = EXCLUDED.enabled,
		     updated_at = EXCLUDED.updated_at
		 RETURNING id::text, tenant_id::text, wallet_id::text, format, pattern, require_memo, auto_refund, EXTRACT(EPOCH FROM refund_after), enabled, created_at, updated_at`,
		policy.ID, policy.TenantID, policy.WalletID, policy.Format, policy.Pattern,
		policy.RequireMemo, policy.AutoRefund, int64(policy.RefundAfter/time.Second), policy.Enabled,
		policy.CreatedAt, policy.UpdatedAt,
	).Scan(&stored.ID, &stored.TenantID, &stored.WalletID, &stored.Format, &stored.Pattern, &stored.RequireMemo, &stored.AutoRefund, &seconds, &stored.Enabled, &stored.CreatedAt, &stored.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("put memo policy for wallet %s: %w", policy.WalletID, err)
	}
	stored.RefundAfter = time.Duration(seconds * float64(time.Second))
	return &stored, nil
}

// DeleteMemoPolicy removes the policy of a wallet.
func (r *MemoPolicyRepo) DeleteMemoPolicy(ctx context.Context, walletID string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM wallet_memo_policies WHERE wallet_id = $1`, walletID)
	if err != nil {
		return fmt.Errorf("delete memo policy for wallet %s: %w", walletID, err)
	}
	return nil
}

// ListMemoPolicies returns every memo policy of a tenant. The read takes a
// share lock so it runs on the primary and shows a policy that was just
// written instead of the replica's older copy.
func (r *MemoPolicyRepo) ListMemoPolicies(ctx context.Context, tenantID string) ([]*domain.MemoPolicy, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id::text, tenant_id::text, wallet_id::text, format, pattern, require_memo, auto_refund, EXTRACT(EPOCH FROM refund_after), enabled, created_at, updated_at
		 FROM wallet_memo_policies WHERE tenant_id = $1 ORDER BY created_at DESC FOR SHARE`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list memo policies: %w", err)
	}
	defer rows.Close()

	policies := make([]*domain.MemoPolicy, 0)
	for rows.Next() {
		var p domain.MemoPolicy
		var seconds float64
		if err := rows.Scan(&p.ID, &p.TenantID, &p.WalletID, &p.Format, &p.Pattern, &p.RequireMemo, &p.AutoRefund, &seconds, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan memo policy: %w", err)
		}
		p.RefundAfter = time.Duration(seconds * float64(time.Second))
		policies = append(policies, &p)
	}
	return policies, rows.Err()
}

// CreateUnmappedDeposit files a deposit in the investigation queue. It
// reports whether the deposit was filed: the indexer replays payments, so a
// deposit that is already queued must not be filed twice.
func (r *MemoPolicyRepo) CreateUnmappedDeposit(ctx context.Context, deposit *domain.UnmappedDeposit) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`INSERT INTO unmapped_deposits (id, tenant_id, wallet_id, mode, tx_hash, source_address, asset, issuer, amount, memo, reason, status, refund_after, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 ON CONFLICT (mode, wallet_id, tx_hash) DO NOTHING`,
		deposit.ID, deposit.TenantID, deposit.WalletID, deposit.Mode, deposit.TxHash,
		deposit.SourceAddress, deposit.Asset, deposit.Issuer, deposit.Amount.String(), deposit.Memo,
		deposit.Reason, deposit.Status, deposit.RefundAfter, deposit.CreatedAt, deposit.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("insert unmapped deposit %s: %w", deposit.TxHash, err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetUnmappedDeposit returns one queued deposit of a tenant.
func (r *MemoPolicyRepo) GetUnmappedDeposit(ctx context.Context, tenantID, id string) (*domain.UnmappedDeposit, error) {
	deposit, err := scanUnmappedDeposit(r.db.QueryRow(ctx, unmappedDepositSelect+` WHERE d.id = $1 AND d.tenant_id = $2`, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get unmapped deposit %s: %w", id, err)
	}
	return deposit, nil
}

// ListUnmappedDeposits returns the investigation queue of a tenant.
func (r *MemoPolicyRepo) ListUnmappedDeposits(ctx context.Context, tenantID, walletID, status string, limit, offset int) ([]*domain.UnmappedDeposit, int, error) {
	var total int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM unmapped_deposits d WHERE d.tenant_id = $1 AND ($2 = '' OR d.wallet_id = $2::uuid) AND ($3 = '' OR d.status = $3)`,
		tenantID, walletID, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count unmapped deposits: %w", err)
	}

	rows, err := r.db.Query(ctx, unmappedDepositSelect+`
		 WHERE d.tenant_id = $1 AND ($2 = '' OR d.wallet_id = $2::uuid) AND ($3 = '' OR d.status = $3)
		 ORDER BY d.created_at DESC LIMIT $4 OFFSET $5`, tenantID, walletID, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list unmapped deposits: %w", err)
	}
	defer rows.Close()

	deposits := make([]*domain.UnmappedDeposit, 0)
	for rows.Next() {
		deposit, err := scanUnmappedDeposit(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan unmapped deposit: %w", err)
		}
		deposits = append(deposits, deposit)
	}
	return deposits, total, rows.Err()
}

// ListUnmappedDepositsDueForRefund returns the pending deposits whose
// refund timeout expired and whose wallet asked for an automated refund.
func (r *MemoPolicyRepo) ListUnmappedDepositsDueForRefund(ctx context.Context, now time.Time, limit int) ([]*domain.UnmappedDeposit, error) {
	rows, err := r.db.Query(ctx, unmappedDepositSelect+`
		 JOIN wallet_memo_policies p ON p.wallet_id = d.wallet_id AND p.tenant_id = d.tenant_id
		 WHERE d.status = 'pending' AND d.refund_after <= $1 AND p.enabled AND p.auto_refund
		 ORDER BY d.refund_after ASC LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list unmapped deposits due for refund: %w", err)
	}
	defer rows.Close()

	deposits := make([]*domain.UnmappedDeposit, 0)
	for rows.Next() {
		deposit, err := scanUnmappedDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unmapped deposit: %w", err)
		}
		deposits = append(deposits, deposit)
	}
	return deposits, rows.Err()
}

// MarkUnmappedDepositRefunded closes a queued deposit that was returned to
// its sender. It reports whether the row was actually closed: the caller has
// to know, because a refund that went out must not be sent again.
func (r *MemoPolicyRepo) MarkUnmappedDepositRefunded(ctx context.Context, id, refundTxHash string) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE unmapped_deposits SET status = 'refunded', refund_tx_hash = $2, failure_reason = '', updated_at = NOW() WHERE id = $1 AND status = 'refunding'`,
		id, refundTxHash)
	if err != nil {
		return false, fmt.Errorf("mark unmapped deposit %s refunded: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimUnmappedDepositForRefund takes ownership of a queued deposit before
// its refund is sent. Only the worker that wins the claim returns the
// deposit to its sender, so two concurrent sweeps cannot pay it twice.
func (r *MemoPolicyRepo) ClaimUnmappedDepositForRefund(ctx context.Context, id string) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE unmapped_deposits SET status = 'refunding', updated_at = NOW() WHERE id = $1 AND status = 'pending'`,
		id)
	if err != nil {
		return false, fmt.Errorf("claim unmapped deposit %s for refund: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordUnmappedDepositRefundFailure stores the reason a refund could not be
// sent. The deposit returns to the pending state so the next sweep retries
// it.
func (r *MemoPolicyRepo) RecordUnmappedDepositRefundFailure(ctx context.Context, id, reason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE unmapped_deposits SET status = 'pending', failure_reason = $2, updated_at = NOW() WHERE id = $1 AND status = 'refunding'`,
		id, reason)
	if err != nil {
		return fmt.Errorf("record refund failure for unmapped deposit %s: %w", id, err)
	}
	return nil
}

// RecordUnmappedDepositRefundUncertain stores the reason a queue entry could
// not be closed although the refund was sent. The deposit stays in the
// refunding state on purpose: it must not become pending again, because the
// money already left the wallet.
func (r *MemoPolicyRepo) RecordUnmappedDepositRefundUncertain(ctx context.Context, id, reason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE unmapped_deposits SET failure_reason = $2, updated_at = NOW() WHERE id = $1 AND status = 'refunding'`,
		id, reason)
	if err != nil {
		return fmt.Errorf("record the uncertain refund of unmapped deposit %s: %w", id, err)
	}
	return nil
}

// MarkUnmappedDepositResolved closes a queued deposit with the operator's
// resolution note. It reports whether the row was still pending.
func (r *MemoPolicyRepo) MarkUnmappedDepositResolved(ctx context.Context, tenantID, id, resolution string) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE unmapped_deposits SET status = 'resolved', resolution = $3, resolved_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND tenant_id = $2 AND status = 'pending'`,
		id, tenantID, resolution)
	if err != nil {
		return false, fmt.Errorf("mark unmapped deposit %s resolved: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// unmappedDepositScanner is the Scan method shared by pgx.Row and pgx.Rows.
type unmappedDepositScanner interface {
	Scan(dest ...interface{}) error
}

// scanUnmappedDeposit reads one queue entry. The amount is scanned as text
// and parsed into a decimal, the way the other repositories read numeric
// columns.
func scanUnmappedDeposit(row unmappedDepositScanner) (*domain.UnmappedDeposit, error) {
	var d domain.UnmappedDeposit
	var amount string
	if err := row.Scan(&d.ID, &d.TenantID, &d.WalletID, &d.Mode, &d.TxHash, &d.SourceAddress,
		&d.Asset, &d.Issuer, &amount, &d.Memo, &d.Reason, &d.Status, &d.RefundAfter,
		&d.RefundTxHash, &d.Resolution, &d.FailureReason, &d.CreatedAt, &d.UpdatedAt, &d.ResolvedAt); err != nil {
		return nil, err
	}
	parsed, err := decimal.NewFromString(amount)
	if err != nil {
		return nil, fmt.Errorf("parse the amount %q of unmapped deposit %s: %w", amount, d.ID, err)
	}
	d.Amount = parsed
	return &d, nil
}

// unmappedDepositSelect is the shared projection of the investigation queue.
const unmappedDepositSelect = `
	SELECT d.id::text, d.tenant_id::text, d.wallet_id::text, d.mode, d.tx_hash, d.source_address,
	       d.asset, d.issuer, d.amount, d.memo, d.reason, d.status, d.refund_after,
	       d.refund_tx_hash, d.resolution, d.failure_reason, d.created_at, d.updated_at, d.resolved_at
	FROM unmapped_deposits d`
