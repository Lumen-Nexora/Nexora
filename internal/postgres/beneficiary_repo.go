package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/beneficiary"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/jackc/pgx/v5"
)

type BeneficiaryRepo struct{ db DB }

func NewBeneficiaryRepo(db DB) *BeneficiaryRepo { return &BeneficiaryRepo{db: db} }

func (r *BeneficiaryRepo) Create(ctx context.Context, b *domain.Beneficiary) error {
	_, err := r.db.Exec(ctx, `INSERT INTO beneficiaries
		(id, tenant_id, mode, account, label, status, cooldown_until, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, b.ID, b.TenantID, b.Mode, b.Account,
		b.Label, b.Status, b.CooldownUntil, b.CreatedAt, b.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "beneficiaries_tenant_mode_account_key") {
			return beneficiary.ErrDuplicate
		}
		return fmt.Errorf("insert beneficiary: %w", err)
	}
	return nil
}

func (r *BeneficiaryRepo) Get(ctx context.Context, id string) (*domain.Beneficiary, error) {
	b := &domain.Beneficiary{}
	err := r.db.QueryRow(ctx, `SELECT id, tenant_id, mode, account, label, status,
		cooldown_until, created_at, updated_at FROM beneficiaries
		WHERE id=$1 AND tenant_id=$2 AND mode=$3`, id, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive)).Scan(
		&b.ID, &b.TenantID, &b.Mode, &b.Account, &b.Label, &b.Status, &b.CooldownUntil, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, beneficiary.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get beneficiary: %w", err)
	}
	return b, nil
}

func (r *BeneficiaryRepo) List(ctx context.Context) ([]*domain.Beneficiary, error) {
	rows, err := r.db.Query(ctx, `SELECT id, tenant_id, mode, account, label, status,
		cooldown_until, created_at, updated_at FROM beneficiaries
		WHERE tenant_id=$1 AND mode=$2 ORDER BY created_at DESC`, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive))
	if err != nil {
		return nil, fmt.Errorf("list beneficiaries: %w", err)
	}
	defer rows.Close()
	items := make([]*domain.Beneficiary, 0)
	for rows.Next() {
		b := &domain.Beneficiary{}
		if err := rows.Scan(&b.ID, &b.TenantID, &b.Mode, &b.Account, &b.Label, &b.Status, &b.CooldownUntil, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan beneficiary: %w", err)
		}
		items = append(items, b)
	}
	return items, rows.Err()
}

func (r *BeneficiaryRepo) Check(ctx context.Context, account string) (configured, active bool, err error) {
	var status string
	err = r.db.QueryRow(ctx, `SELECT status FROM beneficiaries
		WHERE tenant_id=$1 AND mode=$2 AND account=$3`, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive), account).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("check beneficiary: %w", err)
	}
	return true, status == string(domain.BeneficiaryActive), nil
}

func (r *BeneficiaryRepo) Activate(ctx context.Context, id string, now time.Time) (*domain.Beneficiary, error) {
	b := &domain.Beneficiary{}
	err := r.db.QueryRow(ctx, `UPDATE beneficiaries SET status='active', updated_at=$4
		WHERE id=$1 AND tenant_id=$2 AND mode=$3 AND status='pending' AND cooldown_until <= $4
		RETURNING id, tenant_id, mode, account, label, status, cooldown_until, created_at, updated_at`,
		id, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive), now).Scan(
		&b.ID, &b.TenantID, &b.Mode, &b.Account, &b.Label, &b.Status, &b.CooldownUntil, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := r.Get(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		if current.Status == domain.BeneficiaryPending && now.Before(current.CooldownUntil) {
			return nil, beneficiary.ErrCoolingOff
		}
		return nil, beneficiary.ErrNotActive
	}
	if err != nil {
		return nil, fmt.Errorf("activate beneficiary: %w", err)
	}
	return b, nil
}

func (r *BeneficiaryRepo) Revoke(ctx context.Context, id string) error {
	result, err := r.db.Exec(ctx, `UPDATE beneficiaries SET status='revoked', updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2 AND mode=$3 AND status <> 'revoked'`, id, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive))
	if err != nil {
		return fmt.Errorf("revoke beneficiary: %w", err)
	}
	if result.RowsAffected() == 0 {
		return beneficiary.ErrNotFound
	}
	return nil
}
