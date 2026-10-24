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

// RecordNetworkFee inserts one exact on-chain fee. The unique tx_hash index
// makes settlement retries and duplicate Horizon deliveries safe.
func (r *FeeRepo) RecordNetworkFee(ctx context.Context, charge *domain.NetworkFeeCharge) error {
	var rate, usd interface{}
	if charge.XLMUSDRate != nil {
		rate = charge.XLMUSDRate.String()
	}
	if charge.USDAmount != nil {
		usd = charge.USDAmount.String()
	}

	_, err := r.db.Exec(ctx, `
		INSERT INTO network_fee_charges (
			id, transaction_id, tx_hash, tenant_id, sub_account, cost_center,
			allocation, ledger_sequence, ledger_closed_at, fee_charged_stroops,
			xlm_amount, xlm_usd_rate, usd_amount, valued, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (tx_hash) DO NOTHING`,
		charge.ID, nullableUUID(charge.TransactionID), charge.TxHash, nullableUUID(charge.TenantID),
		nullableText(charge.SubAccount), charge.CostCenter, charge.Allocation,
		charge.LedgerSequence, charge.LedgerClosedAt, charge.FeeChargedStroops,
		charge.XLMAmount.String(), rate, usd, charge.Valued, charge.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert network fee charge: %w", err)
	}
	return nil
}

func (r *FeeRepo) GetNetworkFeeByTxHash(ctx context.Context, txHash string) (*domain.NetworkFeeCharge, error) {
	charge := &domain.NetworkFeeCharge{}
	var transactionID, tenantID, subAccount *string
	var xlm, rate, usd *string

	err := r.db.QueryRow(ctx, `
		SELECT id, transaction_id, tx_hash, tenant_id, sub_account, cost_center,
		       allocation, ledger_sequence, ledger_closed_at, fee_charged_stroops,
		       xlm_amount, xlm_usd_rate, usd_amount, valued, created_at
		FROM network_fee_charges
		WHERE tx_hash = $1`, txHash).Scan(
		&charge.ID, &transactionID, &charge.TxHash, &tenantID, &subAccount,
		&charge.CostCenter, &charge.Allocation, &charge.LedgerSequence, &charge.LedgerClosedAt,
		&charge.FeeChargedStroops, &xlm, &rate, &usd, &charge.Valued, &charge.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNetworkFeeNotFound
		}
		return nil, fmt.Errorf("get network fee charge: %w", err)
	}

	charge.TransactionID = transactionID
	charge.TenantID = tenantID
	if subAccount != nil {
		charge.SubAccount = *subAccount
	}
	if xlm != nil {
		value, parseErr := decimal.NewFromString(*xlm)
		if parseErr != nil {
			return nil, fmt.Errorf("parse network fee xlm_amount: %w", parseErr)
		}
		charge.XLMAmount = value
	}
	if rate != nil {
		value, parseErr := decimal.NewFromString(*rate)
		if parseErr != nil {
			return nil, fmt.Errorf("parse network fee xlm_usd_rate: %w", parseErr)
		}
		charge.XLMUSDRate = &value
	}
	if usd != nil {
		value, parseErr := decimal.NewFromString(*usd)
		if parseErr != nil {
			return nil, fmt.Errorf("parse network fee usd_amount: %w", parseErr)
		}
		charge.USDAmount = &value
	}
	return charge, nil
}

func (r *FeeRepo) GetNetworkFeeUsage(ctx context.Context, tenantID string, start, end time.Time) (*domain.NetworkFeeUsage, error) {
	usage := &domain.NetworkFeeUsage{TenantID: tenantID}
	var xlm, usd string

	err := r.db.QueryRow(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(fee_charged_stroops), 0),
		       COALESCE(SUM(xlm_amount), 0)::text,
		       COALESCE(SUM(usd_amount), 0)::text,
		       COUNT(*) FILTER (WHERE valued = FALSE)
		FROM network_fee_charges
		WHERE tenant_id = $1
		  AND ledger_closed_at >= $2
		  AND ledger_closed_at < $3`,
		tenantID, start, end).Scan(
		&usage.TransactionCount, &usage.FeeChargedStroops, &xlm, &usd, &usage.UnvaluedCount,
	)
	if err != nil {
		return nil, fmt.Errorf("get network fee usage: %w", err)
	}

	usage.XLMAmount, err = decimal.NewFromString(xlm)
	if err != nil {
		return nil, fmt.Errorf("parse network fee xlm total: %w", err)
	}
	usage.USDAmount, err = decimal.NewFromString(usd)
	if err != nil {
		return nil, fmt.Errorf("parse network fee usd total: %w", err)
	}
	return usage, nil
}

func (r *FeeRepo) GetMonthlyNetworkFeeCOGS(ctx context.Context, start, end time.Time) (*domain.NetworkFeeCOGSReport, error) {
	rows, err := r.db.Query(ctx, `
		SELECT tenant_id, COALESCE(sub_account, ''), cost_center, allocation,
		       COUNT(*),
		       COALESCE(SUM(fee_charged_stroops), 0),
		       COALESCE(SUM(xlm_amount), 0)::text,
		       COALESCE(SUM(usd_amount), 0)::text,
		       COUNT(*) FILTER (WHERE valued = FALSE)
		FROM network_fee_charges
		WHERE ledger_closed_at >= $1 AND ledger_closed_at < $2
		GROUP BY tenant_id, sub_account, cost_center, allocation
		ORDER BY allocation, tenant_id NULLS FIRST, sub_account, cost_center`,
		start, end)
	if err != nil {
		return nil, fmt.Errorf("get monthly network fee cogs: %w", err)
	}
	defer rows.Close()

	report := &domain.NetworkFeeCOGSReport{
		PeriodStart: start,
		PeriodEnd:   end,
		Lines:       make([]domain.NetworkFeeCOGSLine, 0),
	}
	for rows.Next() {
		line := domain.NetworkFeeCOGSLine{}
		var xlm, usd string
		if err := rows.Scan(
			&line.TenantID, &line.SubAccount, &line.CostCenter, &line.Allocation,
			&line.TransactionCount, &line.FeeChargedStroops, &xlm, &usd, &line.UnvaluedCount,
		); err != nil {
			return nil, fmt.Errorf("scan monthly network fee cogs: %w", err)
		}

		line.XLMAmount, err = decimal.NewFromString(xlm)
		if err != nil {
			return nil, fmt.Errorf("parse monthly network fee xlm: %w", err)
		}
		line.USDAmount, err = decimal.NewFromString(usd)
		if err != nil {
			return nil, fmt.Errorf("parse monthly network fee usd: %w", err)
		}

		report.TransactionCount += line.TransactionCount
		report.FeeChargedStroops += line.FeeChargedStroops
		report.XLMAmount = report.XLMAmount.Add(line.XLMAmount)
		report.USDAmount = report.USDAmount.Add(line.USDAmount)
		report.UnvaluedCount += line.UnvaluedCount
		report.Lines = append(report.Lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate monthly network fee cogs: %w", err)
	}
	return report, nil
}

func nullableText(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}
