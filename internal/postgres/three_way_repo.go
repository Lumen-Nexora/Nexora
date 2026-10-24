package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

type ThreeWayQueryer interface {
	Query(context.Context, string, ...interface{}) (pgx.Rows, error)
}

type ThreeWayRepo struct{ db ThreeWayQueryer }

// Use the primary database: replica lag must not create false missing records.
func NewThreeWayRepo(primary ThreeWayQueryer) *ThreeWayRepo { return &ThreeWayRepo{db: primary} }

func (r *ThreeWayRepo) ListFiatReconciliationTransfers(ctx context.Context, provider string, day time.Time, references []string) ([]*domain.Transaction, error) {
	tenantID := tenant.IDFromContext(ctx)
	mode, ok := tenant.ModeFromContext(ctx)
	if tenantID == "" || !ok {
		return nil, fmt.Errorf("tenant and mode required for reconciliation")
	}
	if references == nil {
		references = []string{}
	}
	rows, err := r.db.Query(ctx, `SELECT id, COALESCE(tx_hash,''), status, fiat_provider_ref, local_currency, local_amount
 FROM transactions WHERE tenant_id=$1 AND mode=$2 AND fiat_rail=$3
 AND ((created_at >= $4 AND created_at < $5) OR fiat_provider_ref=ANY($6::text[]))
 ORDER BY created_at,id LIMIT 10001`, tenantID, mode, provider, day, day.AddDate(0, 0, 1), references)
	if err != nil {
		return nil, fmt.Errorf("query fiat reconciliation transfers: %w", err)
	}
	defer rows.Close()
	result := make([]*domain.Transaction, 0)
	for rows.Next() {
		tx := &domain.Transaction{TenantID: &tenantID, Mode: mode, FiatRail: &provider}
		var amount *string
		if err := rows.Scan(&tx.ID, &tx.TxHash, &tx.Status, &tx.FiatProviderRef, &tx.LocalCurrency, &amount); err != nil {
			return nil, fmt.Errorf("scan fiat reconciliation transfer: %w", err)
		}
		if amount != nil {
			value, err := decimal.NewFromString(*amount)
			if err != nil {
				return nil, fmt.Errorf("parse stored fiat amount: %w", err)
			}
			tx.LocalAmount = &value
		}
		result = append(result, tx)
		if len(result) > 10000 {
			return nil, fmt.Errorf("reconciliation exceeds 10000 transfers; no partial report generated")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read fiat reconciliation transfers: %w", err)
	}
	return result, nil
}
