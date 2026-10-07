package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/jackc/pgx/v5"
)

type WalletBalanceAlertRepo struct{ db DB }

func NewWalletBalanceAlertRepo(db DB) *WalletBalanceAlertRepo {
	return &WalletBalanceAlertRepo{db: db}
}

func (r *WalletBalanceAlertRepo) Create(ctx context.Context, alert *domain.WalletBalanceAlert) error {
	_, err := r.db.Exec(ctx, `INSERT INTO wallet_balance_alerts
		(id, tenant_id, wallet_id, mode, asset_code, asset_issuer, threshold, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		alert.ID, alert.TenantID, alert.WalletID, alert.Mode, alert.AssetCode,
		nullableString(alert.AssetIssuer), alert.Threshold, alert.Status, alert.CreatedAt, alert.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "wallet_balance_alerts_tenant_mode_wallet_id_asset_code_asset_issuer_key") {
			return domain.ErrDuplicateAlert
		}
		return fmt.Errorf("insert wallet balance alert: %w", err)
	}
	return nil
}

func (r *WalletBalanceAlertRepo) Get(ctx context.Context, id string) (*domain.WalletBalanceAlert, error) {
	a := &domain.WalletBalanceAlert{}
	err := r.db.QueryRow(ctx, `SELECT id, tenant_id, wallet_id, mode, asset_code, asset_issuer, threshold, status, created_at, updated_at
		FROM wallet_balance_alerts
		WHERE id=$1 AND tenant_id=$2 AND mode=$3`,
		id, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive)).Scan(
		&a.ID, &a.TenantID, &a.WalletID, &a.Mode, &a.AssetCode, &a.AssetIssuer,
		&a.Threshold, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWalletBalanceAlertNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get wallet balance alert: %w", err)
	}
	return a, nil
}

func (r *WalletBalanceAlertRepo) List(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error) {
	query := `SELECT id, tenant_id, wallet_id, mode, asset_code, asset_issuer, threshold, status, created_at, updated_at
		FROM wallet_balance_alerts
		WHERE tenant_id=$1 AND mode=$2`
	args := []interface{}{tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive)}
	if walletID != "" {
		query += ` AND wallet_id=$3`
		args = append(args, walletID)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list wallet balance alerts: %w", err)
	}
	defer rows.Close()

	items := make([]*domain.WalletBalanceAlert, 0)
	for rows.Next() {
		a := &domain.WalletBalanceAlert{}
		if err := rows.Scan(&a.ID, &a.TenantID, &a.WalletID, &a.Mode, &a.AssetCode, &a.AssetIssuer,
			&a.Threshold, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan wallet balance alert: %w", err)
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (r *WalletBalanceAlertRepo) Update(ctx context.Context, alert *domain.WalletBalanceAlert) error {
	result, err := r.db.Exec(ctx, `UPDATE wallet_balance_alerts
		SET threshold=$4, status=$5, updated_at=$6
		WHERE id=$1 AND tenant_id=$2 AND mode=$3`,
		alert.ID, alert.TenantID, alert.Mode, alert.Threshold, alert.Status, alert.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update wallet balance alert: %w", err)
	}
	if result.RowsAffected() == 0 {
		return domain.ErrWalletBalanceAlertNotFound
	}
	return nil
}

func (r *WalletBalanceAlertRepo) Delete(ctx context.Context, id string) error {
	result, err := r.db.Exec(ctx, `DELETE FROM wallet_balance_alerts
		WHERE id=$1 AND tenant_id=$2 AND mode=$3`,
		id, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive))
	if err != nil {
		return fmt.Errorf("delete wallet balance alert: %w", err)
	}
	if result.RowsAffected() == 0 {
		return domain.ErrWalletBalanceAlertNotFound
	}
	return nil
}

func (r *WalletBalanceAlertRepo) CreateEvent(ctx context.Context, event *domain.WalletBalanceAlertEvent) error {
	_, err := r.db.Exec(ctx, `INSERT INTO wallet_balance_alert_events
		(id, alert_id, wallet_id, asset_code, asset_issuer, balance, threshold, triggered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		event.ID, event.AlertID, event.WalletID, event.AssetCode,
		nullableString(event.AssetIssuer), event.Balance, event.Threshold, event.TriggeredAt)
	if err != nil {
		return fmt.Errorf("insert wallet balance alert event: %w", err)
	}
	return nil
}

func (r *WalletBalanceAlertRepo) ListEvents(ctx context.Context, alertID string, limit int) ([]*domain.WalletBalanceAlertEvent, error) {
	query := `SELECT id, alert_id, wallet_id, asset_code, asset_issuer, balance, threshold, triggered_at
		FROM wallet_balance_alert_events
		WHERE alert_id=$1 ORDER BY triggered_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := r.db.Query(ctx, query, alertID)
	if err != nil {
		return nil, fmt.Errorf("list wallet balance alert events: %w", err)
	}
	defer rows.Close()

	items := make([]*domain.WalletBalanceAlertEvent, 0)
	for rows.Next() {
		e := &domain.WalletBalanceAlertEvent{}
		if err := rows.Scan(&e.ID, &e.AlertID, &e.WalletID, &e.AssetCode, &e.AssetIssuer,
			&e.Balance, &e.Threshold, &e.TriggeredAt); err != nil {
			return nil, fmt.Errorf("scan wallet balance alert event: %w", err)
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

func (r *WalletBalanceAlertRepo) GetActiveAlertsForWallet(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error) {
	query := `SELECT id, tenant_id, wallet_id, mode, asset_code, asset_issuer, threshold, status, created_at, updated_at
		FROM wallet_balance_alerts
		WHERE wallet_id=$1 AND tenant_id=$2 AND mode=$3 AND status='active'`

	rows, err := r.db.Query(ctx, query, walletID, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive))
	if err != nil {
		return nil, fmt.Errorf("get active alerts for wallet: %w", err)
	}
	defer rows.Close()

	items := make([]*domain.WalletBalanceAlert, 0)
	for rows.Next() {
		a := &domain.WalletBalanceAlert{}
		if err := rows.Scan(&a.ID, &a.TenantID, &a.WalletID, &a.Mode, &a.AssetCode, &a.AssetIssuer,
			&a.Threshold, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan wallet balance alert: %w", err)
		}
		items = append(items, a)
	}
	return items, rows.Err()
}
