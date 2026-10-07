package postgres

import (
	"context"
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

type ConversionRepo struct {
	db DB
}

func NewConversionRepo(db DB) *ConversionRepo {
	return &ConversionRepo{db: db}
}

func (r *ConversionRepo) Create(ctx context.Context, c *domain.Conversion) error {
	var minAmtOut *string
	if c.MinAmountOut != nil {
		s := c.MinAmountOut.String()
		minAmtOut = &s
	}

	_, err := r.db.Exec(ctx,
		`INSERT INTO conversions (id, wallet_id, source_asset, dest_asset, source_amount, dest_amount, fee_amount, fee_bps, rate, tx_hash, min_amount_out, max_slippage_bps, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		c.ID, c.WalletID, c.SourceAsset, c.DestAsset,
		c.SourceAmount.String(), c.DestAmount.String(), c.FeeAmount.String(),
		nullableFeeBps(c.FeeBps), c.Rate.String(),
		nullableString(c.TxHash), minAmtOut, c.MaxSlippageBps, c.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert conversion: %w", err)
	}
	return nil
}

func (r *ConversionRepo) ListByWallet(ctx context.Context, walletID string, limit, offset int) ([]*domain.Conversion, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, wallet_id, source_asset, dest_asset, source_amount, dest_amount,
		        COALESCE(fee_amount, '0'), fee_bps, rate, COALESCE(tx_hash,''), min_amount_out, max_slippage_bps, created_at
		 FROM conversions
		 WHERE wallet_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		walletID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list conversions: %w", err)
	}
	defer rows.Close()

	var conversions []*domain.Conversion
	for rows.Next() {
		c := &domain.Conversion{}
		var src, dst, fee, rate string
		var feeBps *int
		var minAmtOut *string
		if err := rows.Scan(&c.ID, &c.WalletID, &c.SourceAsset, &c.DestAsset,
			&src, &dst, &fee, &feeBps, &rate, &c.TxHash, &minAmtOut, &c.MaxSlippageBps, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.SourceAmount, _ = decimal.NewFromString(src)
		c.DestAmount, _ = decimal.NewFromString(dst)
		c.FeeAmount, _ = decimal.NewFromString(fee)
		if feeBps != nil {
			c.FeeBps = *feeBps
		}
		c.Rate, _ = decimal.NewFromString(rate)
		if minAmtOut != nil {
			val, _ := decimal.NewFromString(*minAmtOut)
			c.MinAmountOut = &val
		}
		conversions = append(conversions, c)
	}
	return conversions, rows.Err()
}
