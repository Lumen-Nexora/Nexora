package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/reconcile"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// invariantStatementTimeout bounds the invariant aggregate on the server side
// in addition to the caller's context deadline.
const invariantStatementTimeout = "10s"

// invariantTotalsQuery aggregates, per asset code, everything the zero-sum
// invariant compares:
//
//   - tenant balances: live-mode wallets owned by a tenant that are not
//     registered as pooled reserve wallets;
//   - accumulated fees: fees collected on live-mode transactions minus what
//     treasury sweeps have already moved out of the pool to cold storage;
//   - pooled reserves: balances of the registered pooled reserve wallets.
//
// Everything is summed as NUMERIC and returned as text so no value ever
// passes through a float.
const invariantTotalsQuery = `
WITH tenant_totals AS (
    SELECT b.asset_code AS asset, SUM(b.balance) AS amount
    FROM balances b
    JOIN wallets w ON w.id = b.wallet_id
    WHERE w.tenant_id IS NOT NULL
      AND w.mode = 'live'
      AND NOT EXISTS (SELECT 1 FROM pooled_reserve_wallets p WHERE p.wallet_id = w.id)
    GROUP BY b.asset_code
),
pooled_totals AS (
    SELECT b.asset_code AS asset, SUM(b.balance) AS amount
    FROM balances b
    JOIN pooled_reserve_wallets p ON p.wallet_id = b.wallet_id
    JOIN wallets w ON w.id = b.wallet_id
    WHERE w.mode = 'live'
    GROUP BY b.asset_code
),
fee_totals AS (
    SELECT fc.asset AS asset, SUM(fc.fee_amount) AS amount
    FROM fee_collections fc
    JOIN transactions t ON t.id = fc.transaction_id
    WHERE t.mode = 'live'
    GROUP BY fc.asset
),
swept_totals AS (
    SELECT s.asset AS asset, SUM(s.amount) AS amount
    FROM sweep_log s
    GROUP BY s.asset
),
assets AS (
    SELECT asset FROM tenant_totals
    UNION SELECT asset FROM pooled_totals
    UNION SELECT asset FROM fee_totals
    UNION SELECT asset FROM swept_totals
)
SELECT a.asset,
       COALESCE(tt.amount, 0)::text,
       (COALESCE(ft.amount, 0) - COALESCE(st.amount, 0))::text,
       COALESCE(pt.amount, 0)::text
FROM assets a
LEFT JOIN tenant_totals tt ON tt.asset = a.asset
LEFT JOIN pooled_totals pt ON pt.asset = a.asset
LEFT JOIN fee_totals ft ON ft.asset = a.asset
LEFT JOIN swept_totals st ON st.asset = a.asset
ORDER BY a.asset`

// InvariantRepo implements reconcile.InvariantRepository and
// reconcile.SweepHalter, and lets the sweep workers check for a halt.
type InvariantRepo struct {
	db DB
}

func NewInvariantRepo(db DB) *InvariantRepo {
	return &InvariantRepo{db: db}
}

// GetInvariantTotals reads every aggregate inside one REPEATABLE READ, READ
// ONLY transaction, so all totals come from the same MVCC snapshot. The
// transaction takes no row locks and never blocks settlement writes; the
// statement timeout keeps a pathological run bounded.
func (r *InvariantRepo) GetInvariantTotals(ctx context.Context) (totals []reconcile.InvariantTotals, configured bool, err error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin invariant snapshot: %w", err)
	}
	// Read-only: rolling back is the cheapest way to end the transaction.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
		return nil, false, fmt.Errorf("set invariant snapshot isolation: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '`+invariantStatementTimeout+`'`); err != nil {
		return nil, false, fmt.Errorf("set invariant statement timeout: %w", err)
	}

	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pooled_reserve_wallets)`).Scan(&configured); err != nil {
		return nil, false, fmt.Errorf("check pooled reserve wallets: %w", err)
	}
	if !configured {
		return nil, false, nil
	}

	rows, err := tx.Query(ctx, invariantTotalsQuery)
	if err != nil {
		return nil, true, fmt.Errorf("query invariant totals: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var asset, tenantRaw, feesRaw, pooledRaw string
		if err := rows.Scan(&asset, &tenantRaw, &feesRaw, &pooledRaw); err != nil {
			return nil, true, fmt.Errorf("scan invariant totals: %w", err)
		}
		t := reconcile.InvariantTotals{Asset: asset}
		if t.TenantBalances, err = decimal.NewFromString(tenantRaw); err != nil {
			return nil, true, fmt.Errorf("parse tenant balances for %s: %w", asset, err)
		}
		if t.AccumulatedFees, err = decimal.NewFromString(feesRaw); err != nil {
			return nil, true, fmt.Errorf("parse accumulated fees for %s: %w", asset, err)
		}
		if t.PooledReserves, err = decimal.NewFromString(pooledRaw); err != nil {
			return nil, true, fmt.Errorf("parse pooled reserves for %s: %w", asset, err)
		}
		totals = append(totals, t)
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("read invariant totals: %w", err)
	}
	return totals, true, nil
}

// HaltSweeps records (or keeps) the halt owned by source. The insert is
// idempotent, so repeated violations never stack rows.
func (r *InvariantRepo) HaltSweeps(ctx context.Context, source, reason string) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`INSERT INTO sweep_halts (halted_by, reason) VALUES ($1, $2)
		 ON CONFLICT (halted_by) DO NOTHING`,
		source, reason,
	)
	if err != nil {
		return false, fmt.Errorf("halt sweeps: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResumeSweeps removes only the halt owned by source.
func (r *InvariantRepo) ResumeSweeps(ctx context.Context, source string) (bool, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM sweep_halts WHERE halted_by = $1`, source)
	if err != nil {
		return false, fmt.Errorf("resume sweeps: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SweepsHalted reports whether any component has halted the automated
// sweeps, with the oldest halt's reason. It always reads the primary so a
// lagging replica can never let a sweep slip past a fresh halt.
func (r *InvariantRepo) SweepsHalted(ctx context.Context) (bool, string, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, "", fmt.Errorf("check sweep halts: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var source, reason string
	err = tx.QueryRow(ctx,
		`SELECT halted_by, reason FROM sweep_halts ORDER BY halted_at ASC, halted_by ASC LIMIT 1`,
	).Scan(&source, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check sweep halts: %w", err)
	}
	return true, source + ": " + reason, nil
}
