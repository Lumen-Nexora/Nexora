package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"internal/alerting"
)

// AlertDelay is how long a variance must persist before an operational alert fires.
const AlertDelay = 30 * time.Second

// HorizonClient fetches on-chain balances keyed by account ID.
type HorizonClient interface {
	AccountBalances(ctx context.Context, accountIDs []string) (map[string]BalanceSnapshot, error)
}

// Service runs reconciliation after transactions and persists discrepancies.
type Service struct {
	db       *sql.DB
	horizon  HorizonClient
	detector *Detector
	monitor  *alerting.Monitor

	mu    sync.Mutex
	first map[string]time.Time // dedup key -> first seen at
	now   func() time.Time
}

// NewService constructs a reconciliation service.
func NewService(db *sql.DB, horizon HorizonClient, detector *Detector, monitor *alerting.Monitor) (*Service, error) {
	if db == nil || horizon == nil || detector == nil || monitor == nil {
		return nil, errors.New("reconcile: db, horizon, detector and monitor are required")
	}
	if err := detector.Validate(); err != nil {
		return nil, err
	}
	return &Service{
		db:       db,
		horizon:  horizon,
		detector: detector,
		monitor:  monitor,
		first:    make(map[string]time.Time),
		now:      time.Now,
	}, nil
}

// ReconcileAccounts compares internal balances with Horizon for the given accounts,
// persists new discrepancies, clears resolved ones, and alerts on persistent variance.
func (s *Service) ReconcileAccounts(ctx context.Context, accountIDs []string) ([]Discrepancy, error) {
	internal, err := s.loadInternal(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	onChain, err := s.horizon.AccountBalances(ctx, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("reconcile: horizon: %w", err)
	}
	pending, err := s.loadPending(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	discs := s.detector.Compare(internal, onChain, pending)

	now := s.now()
	seen := make(map[string]struct{}, len(discs))
	for i := range discs {
		d := &discs[i]
		key := dedupKey(d.AccountID, d.AssetCode)
		seen[key] = struct{}{}

		s.mu.Lock()
		first, ok := s.first[key]
		if !ok {
			first = now
			s.first[key] = first
		}
		s.mu.Unlock()
		d.FirstSeenAt = first

		if err := s.persist(ctx, *d); err != nil {
			return nil, err
		}
		if now.Sub(first) > AlertDelay {
			if err := s.monitor.Raise(ctx, alerting.Incident{
				DedupKey: key,
				Summary:  fmt.Sprintf("Unreconciled balance variance for %s/%s", d.AccountID, d.AssetCode),
				Source:   "reconcile",
				Severity: alerting.SeverityCritical,
				Details: map[string]any{
					"variance":   d.Variance.FloatString(7),
					"root_cause": string(d.RootCause),
					"persisted":  now.Sub(first).String(),
				},
				Timestamp: now,
			}); err != nil {
				return discs, fmt.Errorf("reconcile: alert: %w", err)
			}
		}
	}

	// Resolve variances that have cleared.
	s.mu.Lock()
	var cleared []string
	for key := range s.first {
		if _, ok := seen[key]; !ok && s.inScope(key, accountIDs) {
			cleared = append(cleared, key)
		}
	}
	for _, key := range cleared {
		delete(s.first, key)
	}
	s.mu.Unlock()
	for _, key := range cleared {
		if err := s.monitor.Clear(ctx, alerting.Incident{DedupKey: key, Summary: "Balance variance resolved", Source: "reconcile", Severity: alerting.SeverityInfo, Timestamp: now}); err != nil {
			return discs, fmt.Errorf("reconcile: resolve alert: %w", err)
		}
	}
	return discs, nil
}

func (s *Service) inScope(key string, accountIDs []string) bool {
	for _, id := range accountIDs {
		if len(key) > len(id) && key[:len(id)] == id && key[len(id)] == '/' {
			return true
		}
	}
	return false
}

func dedupKey(accountID, assetCode string) string {
	return accountID + "/" + assetCode
}

func (s *Service) loadInternal(ctx context.Context, accountIDs []string) (map[string]BalanceSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, asset_code, balance::text FROM balances WHERE account_id = ANY($1)`,
		pqArray(accountIDs))
	if err != nil {
		return nil, fmt.Errorf("reconcile: query balances: %w", err)
	}
	defer rows.Close()
	out := make(map[string]BalanceSnapshot)
	for rows.Next() {
		var acct, asset, bal string
		if err := rows.Scan(&acct, &asset, &bal); err != nil {
			return nil, fmt.Errorf("reconcile: scan balance: %w", err)
		}
		r, ok := new(big.Rat).SetString(bal)
		if !ok {
			return nil, fmt.Errorf("reconcile: invalid balance %q for %s/%s", bal, acct, asset)
		}
		out[dedupKey(acct, asset)] = BalanceSnapshot{AccountID: acct, AssetCode: asset, Balance: r}
	}
	return out, rows.Err()
}

func (s *Service) loadPending(ctx context.Context, accountIDs []string) (map[string]PendingState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, asset_code,
		       BOOL_OR(status = 'pending') AS has_pending,
		       COALESCE(SUM(CASE WHEN kind = 'unindexed_deposit' THEN amount END), 0)::text,
		       BOOL_OR(kind = 'unauthorized_transfer') AS unauthorized
		FROM ledger_entries
		WHERE account_id = ANY($1) AND status IN ('pending','unindexed_deposit','unauthorized_transfer')
		GROUP BY account_id, asset_code`, pqArray(accountIDs))
	if err != nil {
		return nil, fmt.Errorf("reconcile: query pending: %w", err)
	}
	defer rows.Close()
	out := make(map[string]PendingState)
	for rows.Next() {
		var acct, asset, dep string
		var hasPending, unauth bool
		if err := rows.Scan(&acct, &asset, &hasPending, &dep, &unauth); err != nil {
			return nil, fmt.Errorf("reconcile: scan pending: %w", err)
		}
		p := PendingState{HasPendingSettlement: hasPending, UnauthorizedTransfer: unauth}
		if r, ok := new(big.Rat).SetString(dep); ok && r.Sign() != 0 {
			p.UnindexedDeposit = r
		}
		out[dedupKey(acct, asset)] = p
	}
	return out, rows.Err()
}

func (s *Service) persist(ctx context.Context, d Discrepancy) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO balance_discrepancies
		  (account_id, asset_code, internal_balance, onchain_balance, variance, root_cause, first_seen_at, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (account_id, asset_code) WHERE resolved_at IS NULL
		DO UPDATE SET internal_balance = EXCLUDED.internal_balance,
		              onchain_balance  = EXCLUDED.onchain_balance,
		              variance         = EXCLUDED.variance,
		              root_cause       = EXCLUDED.root_cause,
		              detected_at      = EXCLUDED.detected_at`,
		d.AccountID, d.AssetCode,
		d.Internal.FloatString(18), d.OnChain.FloatString(18), d.Variance.FloatString(18),
		string(d.RootCause), d.FirstSeenAt, d.DetectedAt)
	if err != nil {
		return fmt.Errorf("reconcile: persist discrepancy: %w", err)
	}
	return nil
}

// pqArray encodes a string slice as a Postgres text[] literal.
func pqArray(vals []string) string {
	b := []byte{'{'}
	for i, v := range vals {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		for _, c := range []byte(v) {
			if c == '"' || c == '\\' {
				b = append(b, '\\')
			}
			b = append(b, c)
		}
		b = append(b, '"')
	}
	return string(append(b, '}'))
}
