package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/jackc/pgx/v5"
)

func scanDiscrepancy(row pgx.Row) (*domain.ReconciliationDiscrepancy, error) {
	d := &domain.ReconciliationDiscrepancy{}
	err := row.Scan(&d.ID, &d.TenantID, &d.TransactionID, &d.Category, &d.Status,
		&d.LastAuditLogID, &d.AssignedTo, &d.ResolutionNote, &d.DetectedAt,
		&d.UpdatedAt, &d.ResolvedAt)
	return d, err
}

const discrepancyColumns = `id, tenant_id, transaction_id, category, status,
last_audit_log_id, assigned_to, resolution_note, detected_at, updated_at, resolved_at`

const discrepancyDetailsColumns = `d.id, d.tenant_id, d.transaction_id, d.category, d.status,
d.last_audit_log_id, d.assigned_to, d.resolution_note, d.detected_at, d.updated_at, d.resolved_at,
t.amount::text, t.asset, COALESCE(t.tx_hash, ''), COALESCE(a.details, '')`

func scanDiscrepancyDetails(row pgx.Row) (*domain.ReconciliationDiscrepancy, error) {
	d := &domain.ReconciliationDiscrepancy{}
	err := row.Scan(&d.ID, &d.TenantID, &d.TransactionID, &d.Category, &d.Status,
		&d.LastAuditLogID, &d.AssignedTo, &d.ResolutionNote, &d.DetectedAt,
		&d.UpdatedAt, &d.ResolvedAt, &d.Amount, &d.Asset, &d.StellarTxHash, &d.Details)
	return d, err
}

func (r *TransactionRepo) ListReconciliationDiscrepancies(ctx context.Context, tenantID, status, category string, limit, offset int) ([]*domain.ReconciliationDiscrepancy, int, error) {
	args := []interface{}{tenantID}
	where := " WHERE tenant_id = $1"
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if category != "" {
		args = append(args, category)
		where += fmt.Sprintf(" AND category = $%d", len(args))
	}
	var total int
	if err := r.db.QueryRow(ctx, "SELECT count(*) FROM reconciliation_discrepancies"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reconciliation discrepancies: %w", err)
	}
	args = append(args, limit, offset)
	query := "SELECT " + discrepancyDetailsColumns + " FROM reconciliation_discrepancies d JOIN transactions t ON t.id = d.transaction_id LEFT JOIN ledger_audit_log a ON a.id = d.last_audit_log_id" +
		strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(where, "tenant_id", "d.tenant_id"), "status", "d.status"), "category", "d.category") +
		fmt.Sprintf(" ORDER BY d.detected_at DESC, d.id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list reconciliation discrepancies: %w", err)
	}
	defer rows.Close()
	items := make([]*domain.ReconciliationDiscrepancy, 0)
	for rows.Next() {
		d, err := scanDiscrepancyDetails(rows)
		if err != nil {
			return nil, 0, err
		}
		d.History, err = r.discrepancyHistory(ctx, tenantID, d.ID)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, d)
	}
	return items, total, rows.Err()
}

func (r *TransactionRepo) discrepancyHistory(ctx context.Context, tenantID, id string) ([]domain.ReconciliationDiscrepancyEvent, error) {
	rows, err := r.db.Query(ctx, `SELECT id, actor_id, action, note, assigned_to, created_at
		FROM reconciliation_discrepancy_history
		WHERE tenant_id = $1 AND discrepancy_id = $2
		ORDER BY created_at ASC, id ASC`, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("list discrepancy history: %w", err)
	}
	defer rows.Close()
	history := make([]domain.ReconciliationDiscrepancyEvent, 0)
	for rows.Next() {
		var event domain.ReconciliationDiscrepancyEvent
		if err := rows.Scan(&event.ID, &event.ActorID, &event.Action, &event.Note, &event.AssignedTo, &event.CreatedAt); err != nil {
			return nil, err
		}
		history = append(history, event)
	}
	return history, rows.Err()
}

func (r *TransactionRepo) UpdateReconciliationDiscrepancy(ctx context.Context, tenantID, id, action, note, assignedTo string) (*domain.ReconciliationDiscrepancy, error) {
	actorID := tenant.UserIDFromContext(ctx)
	if actorID == "" {
		actorID = tenant.APIKeyIDFromContext(ctx)
	}
	var actor interface{}
	if actorID != "" {
		actor = actorID
	}
	var assigned interface{}
	if assignedTo != "" {
		assigned = assignedTo
	}
	var result *domain.ReconciliationDiscrepancy
	err := RunInTx(ctx, r.db, func(txCtx context.Context) error {
		db := TxFromContext(txCtx, r.db)
		var err error
		switch action {
		case "acknowledged":
			result, err = scanDiscrepancy(db.QueryRow(txCtx, `UPDATE reconciliation_discrepancies
				SET status = 'acknowledged', updated_at = NOW()
				WHERE tenant_id = $1 AND id = $2 AND status = 'open'
				RETURNING `+discrepancyColumns, tenantID, id))
		case "assigned":
			var member bool
			if err := db.QueryRow(txCtx, `SELECT EXISTS (SELECT 1 FROM organization_members WHERE tenant_id=$1 AND user_id=$2)`, tenantID, assignedTo).Scan(&member); err != nil {
				return err
			}
			if !member {
				return domain.ErrUserNotFound
			}
			result, err = scanDiscrepancy(db.QueryRow(txCtx, `UPDATE reconciliation_discrepancies
				SET assigned_to = $3, updated_at = NOW()
				WHERE tenant_id = $1 AND id = $2 AND status <> 'resolved'
				RETURNING `+discrepancyColumns, tenantID, id, assigned))
		case "annotated":
			result, err = scanDiscrepancy(db.QueryRow(txCtx, `UPDATE reconciliation_discrepancies
				SET resolution_note = CASE WHEN resolution_note = '' THEN $3 ELSE resolution_note || E'\\n' || $3 END,
				    updated_at = NOW()
				WHERE tenant_id = $1 AND id = $2 AND status <> 'resolved'
				RETURNING `+discrepancyColumns, tenantID, id, note))
		case "resolved":
			result, err = scanDiscrepancy(db.QueryRow(txCtx, `UPDATE reconciliation_discrepancies
				SET status = 'resolved', resolution_note = $3, resolved_at = NOW(), updated_at = NOW()
				WHERE tenant_id = $1 AND id = $2 AND status <> 'resolved'
				RETURNING `+discrepancyColumns, tenantID, id, note))
		default:
			return fmt.Errorf("unsupported discrepancy action %q", action)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConcurrentUpdate
		}
		if err != nil {
			return err
		}
		_, err = db.Exec(txCtx, `INSERT INTO reconciliation_discrepancy_history
			(discrepancy_id, tenant_id, actor_id, action, note, assigned_to, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, tenantID, actor, action, note, assigned, time.Now().UTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	result.History, err = r.discrepancyHistory(ctx, tenantID, id)
	return result, err
}

func (r *TransactionRepo) ReconciliationDiscrepancySummary(ctx context.Context, tenantID string) (*domain.ReconciliationDiscrepancySummary, error) {
	rows, err := r.db.Query(ctx, `SELECT status, category, count(*)
		FROM reconciliation_discrepancies WHERE tenant_id = $1
		GROUP BY status, category ORDER BY status, category`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("summarize reconciliation discrepancies: %w", err)
	}
	defer rows.Close()
	summary := &domain.ReconciliationDiscrepancySummary{Counts: make(map[string]int)}
	for rows.Next() {
		var status, category string
		var count int
		if err := rows.Scan(&status, &category, &count); err != nil {
			return nil, err
		}
		summary.Counts[status+":"+category] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.db.QueryRow(ctx, `SELECT count(*), min(detected_at) FROM reconciliation_discrepancies WHERE tenant_id = $1 AND status <> 'resolved'`, tenantID).
		Scan(&summary.UnresolvedCount, &summary.OldestUnresolvedAt); err != nil {
		return nil, fmt.Errorf("load unresolved discrepancy metrics: %w", err)
	}
	return summary, nil
}
