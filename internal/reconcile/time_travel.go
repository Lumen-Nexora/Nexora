package reconcile

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Entry is a single journal ledger entry affecting a wallet balance.
type Entry struct {
	WalletID string
	Sequence int64
	Timestamp time.Time
	Amount *big.Int // signed: positive credit, negative debit
}

// Checkpoint is a persisted balance snapshot at a ledger sequence.
type Checkpoint struct {
	WalletID string
	Sequence int64
	Balance *big.Int
}

// JournalStore provides access to journal entries and checkpoints.
type JournalStore interface {
	// LatestCheckpointAtOrBefore returns the newest checkpoint with sequence <= seq.
	// Returns (nil, nil) if none exists.
	LatestCheckpointAtOrBefore(ctx context.Context, walletID string, seq int64) (*Checkpoint, error)
	// EntriesBetween returns journal entries with fromSeq < sequence <= toSeq, ordered ascending.
	EntriesBetween(ctx context.Context, walletID string, fromSeq, toSeq int64) ([]Entry, error)
	// ResolveTimestampToSequence maps a UNIX timestamp to the last ledger sequence closed at or before it.
	ResolveTimestampToSequence(ctx context.Context, unix int64) (int64, error)
}

// HorizonVerifier fetches authoritative balances from Horizon historical ledger state.
type HorizonVerifier interface {
	BalanceAtLedger(ctx context.Context, walletID string, seq int64) (*big.Int, error)
}

// ErrMismatch is returned when reconstructed balance diverges from Horizon.
var ErrMismatch = errors.New("reconcile: reconstructed balance mismatches horizon state")

// TimeTravel reconstructs point-in-time balances.
type TimeTravel struct {
	store   JournalStore
	horizon HorizonVerifier
}

// NewTimeTravel constructs a TimeTravel engine.
func NewTimeTravel(store JournalStore, horizon HorizonVerifier) *TimeTravel {
	return &TimeTravel{store: store, horizon: horizon}
}

// BalanceAtLedger reconstructs the balance of walletID at ledger sequence seq.
// It starts from the nearest checkpoint (or zero at genesis), replays deltas
// up to seq, then cross-verifies against Horizon.
func (t *TimeTravel) BalanceAtLedger(ctx context.Context, walletID string, seq int64) (*big.Int, error) {
	if walletID == "" {
		return nil, errors.New("reconcile: wallet id required")
	}
	if seq < 0 {
		return nil, fmt.Errorf("reconcile: invalid ledger sequence %d", seq)
	}

	balance := new(big.Int)
	var fromSeq int64 // genesis

	cp, err := t.store.LatestCheckpointAtOrBefore(ctx, walletID, seq)
	if err != nil {
		return nil, fmt.Errorf("reconcile: load checkpoint: %w", err)
	}
	if cp != nil {
		balance.Set(cp.Balance)
		fromSeq = cp.Sequence
	}

	entries, err := t.store.EntriesBetween(ctx, walletID, fromSeq, seq)
	if err != nil {
		return nil, fmt.Errorf("reconcile: replay entries: %w", err)
	}
	for _, e := range entries {
		if e.Amount == nil {
			return nil, fmt.Errorf("reconcile: nil amount at seq %d", e.Sequence)
		}
		balance.Add(balance, e.Amount)
	}

	if t.horizon != nil {
		expected, err := t.horizon.BalanceAtLedger(ctx, walletID, seq)
		if err != nil {
			return nil, fmt.Errorf("reconcile: horizon verify: %w", err)
		}
		if expected.Cmp(balance) != 0 {
			return nil, fmt.Errorf("%w: wallet=%s seq=%d reconstructed=%s horizon=%s",
				ErrMismatch, walletID, seq, balance.String(), expected.String())
		}
	}

	return balance, nil
}

// BalanceAtTimestamp resolves a UNIX timestamp to a ledger sequence and reconstructs the balance.
func (t *TimeTravel) BalanceAtTimestamp(ctx context.Context, walletID string, unix int64) (*big.Int, int64, error) {
	seq, err := t.store.ResolveTimestampToSequence(ctx, unix)
	if err != nil {
		return nil, 0, fmt.Errorf("reconcile: resolve timestamp: %w", err)
	}
	bal, err := t.BalanceAtLedger(ctx, walletID, seq)
	if err != nil {
		return nil, 0, err
	}
	return bal, seq, nil
}
