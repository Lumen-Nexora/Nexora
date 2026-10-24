package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/reconcile"
	"github.com/jackc/pgx/v5"
)

// AutoRemediationRepo implements reconcile.RemediationStore on top of the
// reconciliation_transient_observations and reconciliation_transient_events
// tables. It must be built on the primary database: every method writes or
// takes row locks.
type AutoRemediationRepo struct {
	db DB
}

var _ reconcile.RemediationStore = (*AutoRemediationRepo)(nil)

func NewAutoRemediationRepo(db DB) *AutoRemediationRepo {
	return &AutoRemediationRepo{db: db}
}

const transientObservationColumns = `id, transaction_id, COALESCE(tenant_id::text, ''), mode, tx_hash,
category, status, cycles, first_ledger, last_ledger, detected_at, last_evaluated_at, resolved_at,
reason, trace_hash`

func scanTransientObservation(row pgx.Row) (*reconcile.TransientObservation, error) {
	obs := &reconcile.TransientObservation{}
	var mode, status string
	err := row.Scan(&obs.ID, &obs.TransactionID, &obs.TenantID, &mode, &obs.TxHash,
		&obs.Category, &status, &obs.Cycles, &obs.FirstLedger, &obs.LastLedger,
		&obs.DetectedAt, &obs.LastEvaluatedAt, &obs.ResolvedAt, &obs.Reason, &obs.TraceHash)
	if err != nil {
		return nil, err
	}
	obs.Mode = domain.Mode(mode)
	obs.Status = reconcile.TransientStatus(status)
	return obs, nil
}

func emptyAsNull(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
}

// TrackTransient inserts the observation and its first event in one
// transaction. ON CONFLICT on transaction_id makes it idempotent: a second
// reconciler (or a retry) gets the existing row back with created=false.
func (r *AutoRemediationRepo) TrackTransient(ctx context.Context, obs *reconcile.TransientObservation, event *reconcile.RemediationEvent) (*reconcile.TransientObservation, bool, error) {
	var current *reconcile.TransientObservation
	created := false
	err := RunInTx(ctx, r.db, func(txCtx context.Context) error {
		db := TxFromContext(txCtx, r.db)
		inserted, err := scanTransientObservation(db.QueryRow(txCtx,
			`INSERT INTO reconciliation_transient_observations
			 (id, transaction_id, tenant_id, mode, tx_hash, category, status, cycles,
			  first_ledger, last_ledger, detected_at, last_evaluated_at, reason, trace_hash)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			 ON CONFLICT (transaction_id) DO NOTHING
			 RETURNING `+transientObservationColumns,
			obs.ID, obs.TransactionID, emptyAsNull(obs.TenantID), string(obs.Mode), obs.TxHash,
			obs.Category, string(obs.Status), obs.Cycles, obs.FirstLedger, obs.LastLedger,
			obs.DetectedAt, obs.LastEvaluatedAt, obs.Reason, obs.TraceHash))
		if errors.Is(err, pgx.ErrNoRows) {
			current, err = scanTransientObservation(db.QueryRow(txCtx,
				`SELECT `+transientObservationColumns+`
				 FROM reconciliation_transient_observations WHERE transaction_id = $1`,
				obs.TransactionID))
			if err != nil {
				return fmt.Errorf("load existing transient observation: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert transient observation: %w", err)
		}
		if err := insertRemediationEvent(txCtx, db, event); err != nil {
			return err
		}
		current = inserted
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return current, created, nil
}

// ClaimDueTransients claims observing rows not evaluated within minInterval.
// FOR UPDATE SKIP LOCKED plus the last_evaluated_at stamp means two
// reconcilers never claim the same observation in the same interval.
func (r *AutoRemediationRepo) ClaimDueTransients(ctx context.Context, minInterval time.Duration, limit int) ([]*reconcile.TransientObservation, error) {
	rows, err := r.db.Query(ctx,
		`WITH due AS (
			SELECT id FROM reconciliation_transient_observations
			WHERE status = 'observing'
			  AND last_evaluated_at <= NOW() - ($1 * INTERVAL '1 second')
			ORDER BY detected_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE reconciliation_transient_observations o
		SET last_evaluated_at = NOW()
		FROM due WHERE o.id = due.id
		RETURNING o.id, o.transaction_id, COALESCE(o.tenant_id::text, ''), o.mode, o.tx_hash,
		          o.category, o.status, o.cycles, o.first_ledger, o.last_ledger, o.detected_at,
		          o.last_evaluated_at, o.resolved_at, o.reason, o.trace_hash`,
		minInterval.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim transient observations: %w", err)
	}
	defer rows.Close()

	var claimed []*reconcile.TransientObservation
	for rows.Next() {
		obs, err := scanTransientObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan transient observation: %w", err)
		}
		claimed = append(claimed, obs)
	}
	return claimed, rows.Err()
}

// TransitionTransient is a compare-and-swap on (status = observing,
// trace_hash = prevHash): the losing side of a race gets
// domain.ErrConcurrentUpdate and writes nothing, so an observation is closed
// exactly once and its event chain never forks.
func (r *AutoRemediationRepo) TransitionTransient(ctx context.Context, prevHash string, next *reconcile.TransientObservation, event *reconcile.RemediationEvent) error {
	return RunInTx(ctx, r.db, func(txCtx context.Context) error {
		db := TxFromContext(txCtx, r.db)
		tag, err := db.Exec(txCtx,
			`UPDATE reconciliation_transient_observations
			 SET status = $3, cycles = $4, last_ledger = $5, last_evaluated_at = $6,
			     resolved_at = $7, reason = $8, trace_hash = $9
			 WHERE id = $1 AND status = 'observing' AND trace_hash = $2`,
			next.ID, prevHash, string(next.Status), next.Cycles, next.LastLedger,
			next.LastEvaluatedAt, next.ResolvedAt, next.Reason, next.TraceHash)
		if err != nil {
			return fmt.Errorf("update transient observation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("update transient observation %s: %w", next.ID, domain.ErrConcurrentUpdate)
		}
		return insertRemediationEvent(txCtx, db, event)
	})
}

// ListRemediationEvents returns an observation's event chain, oldest first,
// for verification with reconcile.VerifyRemediationTrace.
func (r *AutoRemediationRepo) ListRemediationEvents(ctx context.Context, observationID string) ([]*reconcile.RemediationEvent, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, observation_id, transaction_id, tx_hash, action, reason, cycle, ledger,
		        prev_hash, hash, created_at
		 FROM reconciliation_transient_events
		 WHERE observation_id = $1
		 ORDER BY created_at ASC, id ASC`, observationID)
	if err != nil {
		return nil, fmt.Errorf("list remediation events: %w", err)
	}
	defer rows.Close()

	var events []*reconcile.RemediationEvent
	for rows.Next() {
		e := &reconcile.RemediationEvent{}
		var action string
		if err := rows.Scan(&e.ID, &e.ObservationID, &e.TransactionID, &e.TxHash, &action, &e.Reason,
			&e.Cycle, &e.Ledger, &e.PrevHash, &e.Hash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan remediation event: %w", err)
		}
		e.Action = reconcile.RemediationAction(action)
		events = append(events, e)
	}
	return events, rows.Err()
}

func insertRemediationEvent(ctx context.Context, db DB, event *reconcile.RemediationEvent) error {
	_, err := db.Exec(ctx,
		`INSERT INTO reconciliation_transient_events
		 (id, observation_id, transaction_id, tx_hash, action, reason, cycle, ledger,
		  prev_hash, hash, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		event.ID, event.ObservationID, event.TransactionID, event.TxHash, string(event.Action),
		event.Reason, event.Cycle, event.Ledger, event.PrevHash, event.Hash, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert remediation event: %w", err)
	}
	return nil
}
