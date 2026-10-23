package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"

	"internal/reconcile"
)

// BalanceRepo implements reconcile.JournalStore backed by PostgreSQL.
//
// Expected schema:
//
//	journal_entries(wallet_id TEXT, ledger_seq BIGINT, closed_at TIMESTAMPTZ, amount NUMERIC(78,0))
//	balance_checkpoints(wallet_id TEXT, ledger_seq BIGINT, balance NUMERIC(78,0), PRIMARY KEY(wallet_id, ledger_seq))
//	ledgers(ledger_seq BIGINT PRIMARY KEY, closed_at TIMESTAMPTZ)
type BalanceRepo struct {
	db *sql.DB
}

// NewBalanceRepo constructs a BalanceRepo.
func NewBalanceRepo(db *sql.DB) *BalanceRepo {
	return &BalanceRepo{db: db}
}

// LatestCheckpointAtOrBefore returns the newest checkpoint at or before seq.
func (r *BalanceRepo) LatestCheckpointAtOrBefore(ctx context.Context, walletID string, seq int64) (*reconcile.Checkpoint, error) {
	const q = `
		SELECT ledger_seq, balance::text
		FROM balance_checkpoints
		WHERE wallet_id = $1 AND ledger_seq <= $2
		ORDER BY ledger_seq DESC
		LIMIT 1`
	var (
		cpSeq   int64
		balText string
	)
	err := r.db.QueryRowContext(ctx, q, walletID, seq).Scan(&cpSeq, &balText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query checkpoint: %w", err)
	}
	bal, ok := new(big.Int).SetString(balText, 10)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint balance %q", balText)
	}
	return &reconcile.Checkpoint{WalletID: walletID, Sequence: cpSeq, Balance: bal}, nil
}

// EntriesBetween returns journal entries with fromSeq < ledger_seq <= toSeq, ascending.
func (r *BalanceRepo) EntriesBetween(ctx context.Context, walletID string, fromSeq, toSeq int64) ([]reconcile.Entry, error) {
	const q = `
		SELECT ledger_seq, closed_at, amount::text
		FROM journal_entries
		WHERE wallet_id = $1 AND ledger_seq > $2 AND ledger_seq <= $3
		ORDER BY ledger_seq ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, q, walletID, fromSeq, toSeq)
	if err != nil {
		return nil, fmt.Errorf("query entries: %w", err)
	}
	defer rows.Close()

	var out []reconcile.Entry
	for rows.Next() {
		var (
			e       reconcile.Entry
			amtText string
			ts      time.Time
		)
		if err := rows.Scan(&e.Sequence, &ts, &amtText); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		amt, ok := new(big.Int).SetString(amtText, 10)
		if !ok {
			return nil, fmt.Errorf("invalid entry amount %q at seq %d", amtText, e.Sequence)
		}
		e.WalletID = walletID
		e.Timestamp = ts
		e.Amount = amt
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entries: %w", err)
	}
	return out, nil
}

// ResolveTimestampToSequence returns the greatest ledger sequence closed at or before unix.
func (r *BalanceRepo) ResolveTimestampToSequence(ctx context.Context, unix int64) (int64, error) {
	const q = `
		SELECT ledger_seq
		FROM ledgers
		WHERE closed_at <= to_timestamp($1)
		ORDER BY ledger_seq DESC
		LIMIT 1`
	var seq int64
	err := r.db.QueryRowContext(ctx, q, unix).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("no ledger closed at or before unix %d", unix)
	}
	if err != nil {
		return 0, fmt.Errorf("query ledger by time: %w", err)
	}
	return seq, nil
}

// WriteCheckpoint persists a balance checkpoint for later delta replay.
func (r *BalanceRepo) WriteCheckpoint(ctx context.Context, cp reconcile.Checkpoint) error {
	if cp.Balance == nil {
		return errors.New("nil checkpoint balance")
	}
	const q = `
		INSERT INTO balance_checkpoints (wallet_id, ledger_seq, balance)
		VALUES ($1, $2, $3::numeric)
		ON CONFLICT (wallet_id, ledger_seq) DO UPDATE SET balance = EXCLUDED.balance`
	_, err := r.db.ExecContext(ctx, q, cp.WalletID, cp.Sequence, cp.Balance.String())
	if err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	return nil
}
