package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Auto-remediation of transient settlement discrepancies (#120).
//
// The confirmed-transaction pass used to flag a transaction the moment
// Horizon returned 404 for its hash. Right after confirmation that is often
// just Horizon ingestion lagging the database by a ledger or two, so the
// reconciler paged an operator for something that fixed itself a few seconds
// later.
//
// The auto-remediator narrows that one case. A confirmed transaction that is
// missing on Horizon and was created within TransientWindow is recorded as a
// transient observation instead of being flagged. The observation is then
// re-evaluated once per ledger close:
//
//   - Horizon now has the transaction: it goes through the exact same
//     verification as any other confirmed transaction (source, destination,
//     asset identity, amount, fee leg). Only a full pass resolves the
//     observation; any mismatch is flagged through the existing path and the
//     observation is escalated. Resolution never moves funds or changes the
//     transaction's status — it only records that reconciliation passed.
//   - Horizon still returns 404: the consecutive cycle count goes up. Once the
//     discrepancy has persisted for more than MaxCycles consecutive cycles, or
//     the observation is older than MaxAge, it is escalated through
//     flagMissingOnChain, exactly as before this feature existed.
//
// Anything else (mismatches, unsuccessful transactions, transactions outside
// the window, a transaction whose state changed underneath the observation)
// is never deferred and never auto-resolved.
//
// Every action is appended to a per-observation SHA-256 hash chain in the
// RemediationStore and mirrored to the audit trail when one is wired.

// RemediationCategoryHorizonLag names the only discrepancy class the
// auto-remediator handles: a confirmed transaction Horizon has not ingested.
const RemediationCategoryHorizonLag = "horizon_ingestion_lag"

// remediationActor is the actor recorded on audit events emitted here.
const remediationActor = "reconcile.auto_remediator"

// RemediationTraceAlgorithm documents the event hash formula so exported
// traces can be re-verified by external tooling.
const RemediationTraceAlgorithm = "sha256(prev_hash | canonical_event_json)"

// RemediationGenesisHash is the prev_hash of the first event in a chain.
var RemediationGenesisHash = strings.Repeat("0", sha256.Size*2)

const (
	// DefaultTransientWindow bounds how old a confirmed transaction may be
	// for a Horizon 404 to be treated as ingestion lag.
	DefaultTransientWindow = 10 * time.Minute
	// DefaultRemediationMaxCycles is how many consecutive evaluation cycles a
	// discrepancy may persist before it is escalated.
	DefaultRemediationMaxCycles = 3
	// DefaultRemediationMaxAge is a hard deadline after which an observation
	// is escalated even if too few cycles were counted (e.g. Horizon stopped
	// closing ledgers).
	DefaultRemediationMaxAge = 15 * time.Minute
	// DefaultRemediationEvaluationInterval is the minimum gap between two
	// evaluations of the same observation.
	DefaultRemediationEvaluationInterval = 30 * time.Second

	defaultRemediationBatchSize = 100
)

// AutoRemediationConfig tunes the auto-remediator. Zero or negative values
// fall back to the defaults above.
type AutoRemediationConfig struct {
	TransientWindow    time.Duration
	MaxCycles          int
	MaxAge             time.Duration
	EvaluationInterval time.Duration
	BatchSize          int
}

func (c AutoRemediationConfig) withDefaults() AutoRemediationConfig {
	if c.TransientWindow <= 0 {
		c.TransientWindow = DefaultTransientWindow
	}
	if c.MaxCycles <= 0 {
		c.MaxCycles = DefaultRemediationMaxCycles
	}
	if c.MaxAge <= 0 {
		c.MaxAge = DefaultRemediationMaxAge
	}
	if c.EvaluationInterval <= 0 {
		c.EvaluationInterval = DefaultRemediationEvaluationInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultRemediationBatchSize
	}
	return c
}

// TransientStatus is the lifecycle state of a transient observation.
type TransientStatus string

const (
	TransientObserving TransientStatus = "observing"
	TransientResolved  TransientStatus = "resolved"
	TransientEscalated TransientStatus = "escalated"
)

// RemediationAction names one entry in an observation's event chain.
type RemediationAction string

const (
	RemediationDetected     RemediationAction = "detected"
	RemediationStillMissing RemediationAction = "still_missing"
	RemediationResolved     RemediationAction = "resolved"
	RemediationEscalated    RemediationAction = "escalated"
)

// TransientObservation tracks one confirmed transaction that Horizon did not
// know about when it was reconciled.
type TransientObservation struct {
	ID            string
	TransactionID string
	TenantID      string
	Mode          domain.Mode
	TxHash        string
	Category      string
	Status        TransientStatus
	// Cycles counts the consecutive evaluation cycles, including the one that
	// detected it, in which the discrepancy was present.
	Cycles          int
	FirstLedger     int64
	LastLedger      int64
	DetectedAt      time.Time
	LastEvaluatedAt time.Time
	ResolvedAt      *time.Time
	Reason          string
	// TraceHash is the hash of the latest event in the observation's chain.
	TraceHash string
}

// RemediationEvent is one hash-chained entry in an observation's trace.
type RemediationEvent struct {
	ID            string
	ObservationID string
	TransactionID string
	TxHash        string
	Action        RemediationAction
	Reason        string
	Cycle         int
	Ledger        int64
	PrevHash      string
	Hash          string
	CreatedAt     time.Time
}

// RemediationStore persists observations and their event chains. It is
// implemented by postgres.AutoRemediationRepo.
type RemediationStore interface {
	// TrackTransient inserts obs together with its first event unless the
	// transaction is already tracked, in which case the existing observation
	// is returned with created=false and nothing is written.
	TrackTransient(ctx context.Context, obs *TransientObservation, event *RemediationEvent) (current *TransientObservation, created bool, err error)
	// ClaimDueTransients claims up to limit observing rows that have not been
	// evaluated within minInterval and stamps their last_evaluated_at, so
	// concurrent reconcilers never evaluate the same row at the same time.
	ClaimDueTransients(ctx context.Context, minInterval time.Duration, limit int) ([]*TransientObservation, error)
	// TransitionTransient atomically replaces the observation with next and
	// appends event, provided the stored row is still observing with
	// trace_hash == prevHash. Otherwise it returns domain.ErrConcurrentUpdate
	// and writes nothing.
	TransitionTransient(ctx context.Context, prevHash string, next *TransientObservation, event *RemediationEvent) error
}

// TransactionLoader reloads a transaction before an observation is acted on.
// Implemented by postgres.TransactionRepo.
type TransactionLoader interface {
	GetByID(ctx context.Context, id string) (*domain.Transaction, error)
}

// LedgerSource reports the latest ledger Horizon has ingested, so an
// evaluation cycle is only counted once a new ledger has closed.
type LedgerSource interface {
	LatestLedger(ctx context.Context) (int64, error)
}

// RemediationAuditTrail mirrors remediation actions into the platform audit
// log. audit.Service satisfies it.
type RemediationAuditTrail interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
}

// AutoRemediator holds the auto-remediation configuration and dependencies.
// It is attached to a Service with WithAutoRemediator.
type AutoRemediator struct {
	cfg      AutoRemediationConfig
	store    RemediationStore
	txLoader TransactionLoader
	ledgers  LedgerSource
	audit    RemediationAuditTrail
	now      func() time.Time
}

// NewAutoRemediator builds an auto-remediator. store and txLoader are
// required; without them the remediator is inert and every Horizon 404 is
// flagged immediately, as before.
func NewAutoRemediator(cfg AutoRemediationConfig, store RemediationStore, txLoader TransactionLoader) *AutoRemediator {
	return &AutoRemediator{
		cfg:      cfg.withDefaults(),
		store:    store,
		txLoader: txLoader,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// WithLedgerSource makes evaluation cycles follow ledger closes. Without one,
// every evaluation pass counts as a cycle.
func (r *AutoRemediator) WithLedgerSource(source LedgerSource) *AutoRemediator {
	r.ledgers = source
	return r
}

// WithAuditTrail mirrors every remediation action into the audit log.
func (r *AutoRemediator) WithAuditTrail(trail RemediationAuditTrail) *AutoRemediator {
	r.audit = trail
	return r
}

// Config returns the effective configuration, defaults applied.
func (r *AutoRemediator) Config() AutoRemediationConfig { return r.cfg }

// WithAutoRemediator attaches the transient-discrepancy auto-remediator. When
// nil, a Horizon 404 on a confirmed transaction is flagged immediately.
func (s *Service) WithAutoRemediator(r *AutoRemediator) *Service {
	s.autoRemediator = r
	return s
}

func (r *AutoRemediator) usable() bool {
	return r != nil && r.store != nil && r.txLoader != nil
}

// isTransientCandidate reports whether a Horizon 404 for tx may be ingestion
// lag. Only confirmed transactions with a hash, created no more than
// TransientWindow ago, qualify.
func (r *AutoRemediator) isTransientCandidate(tx *domain.Transaction, now time.Time) bool {
	if tx == nil || tx.Status != domain.StatusConfirmed || tx.TxHash == "" || tx.CreatedAt.IsZero() {
		return false
	}
	return now.Sub(tx.CreatedAt) <= r.cfg.TransientWindow
}

// deferTransientMissing is called when Horizon returns 404 for a confirmed
// transaction. It returns true when the auto-remediator has taken ownership
// of the discrepancy, in which case the caller must not flag it. Any failure
// returns false so the caller falls back to flagging immediately.
func (s *Service) deferTransientMissing(ctx context.Context, tx *domain.Transaction) bool {
	r := s.autoRemediator
	if !r.usable() {
		return false
	}
	deferred, err := r.trackMissing(ctx, tx)
	if err != nil {
		log.Error().Err(err).Str("tx_id", tx.ID).Str("tx_hash", tx.TxHash).
			Msg("reconcile: auto-remediation could not track discrepancy, flagging immediately")
		return false
	}
	return deferred
}

func (r *AutoRemediator) trackMissing(ctx context.Context, tx *domain.Transaction) (bool, error) {
	now := r.now()
	if !r.isTransientCandidate(tx, now) {
		log.Info().Str("tx_id", tx.ID).Str("tx_hash", tx.TxHash).
			Dur("age", now.Sub(tx.CreatedAt)).Dur("transient_window", r.cfg.TransientWindow).
			Msg("reconcile: discrepancy is outside the transient window, not auto-remediable")
		return false, nil
	}

	// The ledger baseline is best-effort: without it the first evaluation
	// simply counts as a new ledger, and MaxAge still bounds the deferral.
	var ledger int64
	if r.ledgers != nil {
		latest, err := r.ledgers.LatestLedger(ctx)
		if err != nil {
			log.Warn().Err(err).Str("tx_id", tx.ID).Msg("reconcile: could not read latest ledger for transient baseline")
		} else {
			ledger = latest
		}
	}

	tenantID := ""
	if tx.TenantID != nil {
		tenantID = *tx.TenantID
	}
	mode := tx.Mode
	if mode == "" {
		mode = domain.ModeLive
	}
	obs := &TransientObservation{
		ID:              uuid.New().String(),
		TransactionID:   tx.ID,
		TenantID:        tenantID,
		Mode:            mode,
		TxHash:          tx.TxHash,
		Category:        RemediationCategoryHorizonLag,
		Status:          TransientObserving,
		Cycles:          1,
		FirstLedger:     ledger,
		LastLedger:      ledger,
		DetectedAt:      now,
		LastEvaluatedAt: now,
		TraceHash:       RemediationGenesisHash,
	}
	event := r.newEvent(obs, RemediationDetected, "confirmed transaction not yet visible on Horizon", ledger)
	obs.TraceHash = event.Hash

	current, created, err := r.store.TrackTransient(ctx, obs, event)
	if err != nil {
		return false, fmt.Errorf("track transient discrepancy: %w", err)
	}
	if !created {
		if current != nil && current.Status == TransientObserving && current.TxHash == tx.TxHash {
			log.Debug().Str("tx_id", tx.ID).Str("observation_id", current.ID).
				Msg("reconcile: discrepancy already under auto-remediation observation")
			return true, nil
		}
		status := ""
		if current != nil {
			status = string(current.Status)
		}
		log.Warn().Str("tx_id", tx.ID).Str("previous_status", status).
			Msg("reconcile: discrepancy was already auto-remediated or escalated once, not deferring again")
		return false, nil
	}

	log.Warn().Str("tx_id", tx.ID).Str("tx_hash", tx.TxHash).Str("observation_id", obs.ID).
		Int64("ledger", ledger).Str("trace_hash", obs.TraceHash).
		Msg("reconcile: transient discrepancy deferred for auto-remediation")
	r.recordAudit(ctx, obs, event)
	return true, nil
}

// AutoRemediationResult summarises one RunAutoRemediation pass.
type AutoRemediationResult struct {
	Evaluated int
	Resolved  int
	Escalated int
	Observing int
	Failed    int
}

// RunAutoRemediation re-evaluates every due transient observation. It is safe
// to run concurrently from several workers: rows are claimed with SKIP
// LOCKED and every transition is a compare-and-swap on the trace hash, so an
// observation is resolved or escalated at most once.
func (s *Service) RunAutoRemediation(ctx context.Context) (AutoRemediationResult, error) {
	var result AutoRemediationResult
	r := s.autoRemediator
	if !r.usable() {
		return result, nil
	}

	for {
		batch, err := r.store.ClaimDueTransients(ctx, r.cfg.EvaluationInterval, r.cfg.BatchSize)
		if err != nil {
			return result, fmt.Errorf("claim transient observations: %w", err)
		}
		for _, obs := range batch {
			result.Evaluated++
			status, evalErr := s.evaluateTransient(ctx, obs)
			if evalErr != nil {
				if errors.Is(evalErr, domain.ErrConcurrentUpdate) {
					log.Info().Str("observation_id", obs.ID).Str("tx_id", obs.TransactionID).
						Msg("reconcile: transient observation already handled by another reconciler")
					continue
				}
				result.Failed++
				log.Error().Err(evalErr).Str("observation_id", obs.ID).Str("tx_id", obs.TransactionID).
					Msg("reconcile: transient observation evaluation failed, will retry next cycle")
				continue
			}
			switch status {
			case TransientResolved:
				result.Resolved++
			case TransientEscalated:
				result.Escalated++
			default:
				result.Observing++
			}
		}
		if len(batch) < r.cfg.BatchSize {
			break
		}
	}

	if result.Evaluated > 0 {
		log.Info().Int("evaluated", result.Evaluated).Int("resolved", result.Resolved).
			Int("escalated", result.Escalated).Int("observing", result.Observing).Int("failed", result.Failed).
			Msg("reconcile: auto-remediation pass complete")
	}
	return result, nil
}

// evaluateTransient runs one evaluation cycle for an observation and returns
// its resulting status. An error leaves the observation untouched so it is
// retried on the next cycle.
func (s *Service) evaluateTransient(ctx context.Context, obs *TransientObservation) (TransientStatus, error) {
	r := s.autoRemediator
	now := r.now()
	ctx = observationScope(ctx, obs)
	expired := now.Sub(obs.DetectedAt) > r.cfg.MaxAge

	tx, err := r.txLoader.GetByID(ctx, obs.TransactionID)
	if err != nil {
		if errors.Is(err, domain.ErrTransactionNotFound) {
			return s.escalateTransient(ctx, obs, nil, obs.LastLedger, "transaction no longer exists", false)
		}
		if expired {
			return s.escalateTransient(ctx, obs, nil, obs.LastLedger,
				fmt.Sprintf("unresolved after %s and the transaction could not be reloaded: %v", r.cfg.MaxAge, err), false)
		}
		return TransientObserving, fmt.Errorf("load transaction %s: %w", obs.TransactionID, err)
	}
	// Never act on a transaction that moved on while it was being observed:
	// someone (or something) else now owns it.
	if tx.Status != domain.StatusConfirmed || tx.TxHash != obs.TxHash {
		return s.escalateTransient(ctx, obs, tx, obs.LastLedger,
			fmt.Sprintf("transaction changed while under observation (status=%s)", tx.Status), false)
	}

	ledger := obs.LastLedger
	if r.ledgers != nil {
		latest, ledgerErr := r.ledgers.LatestLedger(ctx)
		switch {
		case ledgerErr != nil && !expired:
			return TransientObserving, fmt.Errorf("read latest ledger: %w", ledgerErr)
		case ledgerErr == nil && latest > obs.LastLedger:
			ledger = latest
		case !expired:
			// No ledger has closed since the last evaluation, so Horizon
			// cannot have caught up yet and this is not a new cycle.
			log.Debug().Str("observation_id", obs.ID).Int64("ledger", latest).
				Msg("reconcile: no new ledger since last transient evaluation")
			return TransientObserving, nil
		}
	}

	horizonTx, err := stellar.TransactionDetailWithContext(ctx, s.stellar, obs.TxHash)
	if err != nil {
		if !stellar.IsNotFound(err) {
			if expired {
				return s.escalateTransient(ctx, obs, tx, ledger,
					fmt.Sprintf("unresolved after %s and Horizon is unavailable: %v", r.cfg.MaxAge, err), false)
			}
			return TransientObserving, fmt.Errorf("fetch transaction detail for %s: %w", obs.TxHash, err)
		}

		cycles := obs.Cycles + 1
		if cycles > r.cfg.MaxCycles || expired {
			return s.escalateTransient(ctx, obs, tx, ledger,
				fmt.Sprintf("still missing on Horizon after %d consecutive evaluation cycles (%s)", cycles,
					now.Sub(obs.DetectedAt).Round(time.Second)), true)
		}
		next := *obs
		next.Cycles = cycles
		next.LastLedger = ledger
		next.LastEvaluatedAt = now
		event := r.newEvent(&next, RemediationStillMissing, "still missing on Horizon", ledger)
		next.TraceHash = event.Hash
		if err := r.store.TransitionTransient(ctx, obs.TraceHash, &next, event); err != nil {
			return TransientObserving, fmt.Errorf("record transient cycle: %w", err)
		}
		log.Warn().Str("observation_id", obs.ID).Str("tx_id", obs.TransactionID).
			Int("cycle", cycles).Int("max_cycles", r.cfg.MaxCycles).Int64("ledger", ledger).
			Msg("reconcile: transient discrepancy still present")
		r.recordAudit(ctx, &next, event)
		return TransientObserving, nil
	}

	// Horizon has the transaction now. It is resolved only if it passes the
	// same full verification as any confirmed transaction; a failing check is
	// flagged by verifyHorizonTransaction itself through the existing path.
	verified, err := s.verifyHorizonTransaction(ctx, tx, horizonTx)
	if err != nil {
		return TransientObserving, fmt.Errorf("verify transaction %s: %w", tx.ID, err)
	}
	if !verified {
		return s.closeTransient(ctx, obs, TransientEscalated, ledger,
			"visible on Horizon but failed reconciliation checks; flagged for manual review")
	}
	return s.closeTransient(ctx, obs, TransientResolved, ledger,
		fmt.Sprintf("self-corrected: visible on Horizon in ledger %d after %d cycle(s)", horizonTx.Ledger, obs.Cycles))
}

// escalateTransient hands an observation to a human. When flagMissing is set
// and the transaction is still confirmed, it is flagged through
// flagMissingOnChain — the same reconciliation_failed status, not_found
// audit entry, and critical alert as an un-deferred 404. The transition is
// written first, so a reconciler that lost the race never flags twice.
func (s *Service) escalateTransient(ctx context.Context, obs *TransientObservation, tx *domain.Transaction, ledger int64, reason string, flagMissing bool) (TransientStatus, error) {
	if _, err := s.closeTransient(ctx, obs, TransientEscalated, ledger, reason); err != nil {
		return TransientObserving, err
	}
	if flagMissing && tx != nil && tx.Status == domain.StatusConfirmed {
		if err := s.flagMissingOnChain(ctx, tx); err != nil {
			s.alerting.Critical(ctx, "Auto-Remediation Escalation Failed",
				fmt.Sprintf("Transaction %s (hash: %s) was escalated (%s) but could not be flagged: %v. Manual review required.",
					obs.TransactionID, obs.TxHash, reason, err))
			return TransientEscalated, fmt.Errorf("flag escalated transaction %s: %w", obs.TransactionID, err)
		}
		return TransientEscalated, nil
	}
	s.alerting.Warning(ctx, "Reconciliation Discrepancy Escalated",
		fmt.Sprintf("Transaction %s (hash: %s) was not auto-remediated: %s. Manual review required.",
			obs.TransactionID, obs.TxHash, reason))
	return TransientEscalated, nil
}

// closeTransient moves an observation to a final status and appends the
// matching event to its chain.
func (s *Service) closeTransient(ctx context.Context, obs *TransientObservation, status TransientStatus, ledger int64, reason string) (TransientStatus, error) {
	r := s.autoRemediator
	now := r.now()
	next := *obs
	next.Status = status
	next.LastLedger = ledger
	next.LastEvaluatedAt = now
	next.ResolvedAt = &now
	next.Reason = reason

	action := RemediationEscalated
	if status == TransientResolved {
		action = RemediationResolved
	}
	event := r.newEvent(&next, action, reason, ledger)
	next.TraceHash = event.Hash
	if err := r.store.TransitionTransient(ctx, obs.TraceHash, &next, event); err != nil {
		return TransientObserving, fmt.Errorf("record transient %s: %w", status, err)
	}

	logEvent := log.Warn()
	if status == TransientResolved {
		logEvent = log.Info()
	}
	logEvent.Str("observation_id", obs.ID).Str("tx_id", obs.TransactionID).Str("tx_hash", obs.TxHash).
		Str("status", string(status)).Int("cycles", obs.Cycles).Int64("ledger", ledger).
		Str("reason", reason).Str("trace_hash", next.TraceHash).
		Msg("reconcile: transient discrepancy " + string(action))
	r.recordAudit(ctx, &next, event)
	return status, nil
}

func (r *AutoRemediator) newEvent(obs *TransientObservation, action RemediationAction, reason string, ledger int64) *RemediationEvent {
	event := &RemediationEvent{
		ID:            uuid.New().String(),
		ObservationID: obs.ID,
		TransactionID: obs.TransactionID,
		TxHash:        obs.TxHash,
		Action:        action,
		Reason:        reason,
		Cycle:         obs.Cycles,
		Ledger:        ledger,
		PrevHash:      obs.TraceHash,
		CreatedAt:     r.now().UTC(),
	}
	event.Hash = RemediationEventHash(event)
	return event
}

// canonicalRemediationEvent fixes the byte layout hashed into an event.
type canonicalRemediationEvent struct {
	ID            string `json:"id"`
	ObservationID string `json:"observation_id"`
	TransactionID string `json:"transaction_id"`
	TxHash        string `json:"tx_hash"`
	Action        string `json:"action"`
	Reason        string `json:"reason"`
	Cycle         int    `json:"cycle"`
	Ledger        int64  `json:"ledger"`
	CreatedAt     string `json:"created_at"`
}

// RemediationEventHash computes sha256(prev_hash | canonical_event_json) for
// an event. Every field except Hash itself is covered.
func RemediationEventHash(e *RemediationEvent) string {
	payload, err := json.Marshal(canonicalRemediationEvent{
		ID:            e.ID,
		ObservationID: e.ObservationID,
		TransactionID: e.TransactionID,
		TxHash:        e.TxHash,
		Action:        string(e.Action),
		Reason:        e.Reason,
		Cycle:         e.Cycle,
		Ledger:        e.Ledger,
		CreatedAt:     e.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		// Marshalling a struct of strings and integers cannot fail; hashing
		// the bare prev hash keeps the chain well-formed regardless.
		payload = nil
	}
	material := make([]byte, 0, len(e.PrevHash)+1+len(payload))
	material = append(material, e.PrevHash...)
	material = append(material, '|')
	material = append(material, payload...)
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

// VerifyRemediationTrace re-computes an observation's event chain, oldest
// first, and reports the first event that does not link or hash correctly.
func VerifyRemediationTrace(events []*RemediationEvent) error {
	prev := RemediationGenesisHash
	for i, e := range events {
		if e.PrevHash != prev {
			return fmt.Errorf("remediation event %d (%s): prev_hash does not link to the previous event", i, e.ID)
		}
		if want := RemediationEventHash(e); e.Hash != want {
			return fmt.Errorf("remediation event %d (%s): hash mismatch", i, e.ID)
		}
		prev = e.Hash
	}
	return nil
}

func (r *AutoRemediator) recordAudit(ctx context.Context, obs *TransientObservation, event *RemediationEvent) {
	if r.audit == nil {
		return
	}
	auditEvent := &domain.AuditEvent{
		TenantID:     obs.TenantID,
		Mode:         obs.Mode,
		ActorType:    "system",
		ActorID:      remediationActor,
		Action:       "reconciliation.auto_remediation." + string(event.Action),
		ResourceType: "transaction",
		ResourceID:   obs.TransactionID,
		Metadata: map[string]interface{}{
			"observation_id":  obs.ID,
			"event_id":        event.ID,
			"tx_hash":         obs.TxHash,
			"category":        obs.Category,
			"status":          string(obs.Status),
			"cycle":           event.Cycle,
			"ledger":          event.Ledger,
			"reason":          event.Reason,
			"prev_trace_hash": event.PrevHash,
			"trace_hash":      event.Hash,
			"trace_algorithm": RemediationTraceAlgorithm,
		},
		CreatedAt: event.CreatedAt,
	}
	if err := r.audit.Record(ctx, auditEvent); err != nil {
		// The hash-chained event is already committed with the transition;
		// this mirror is best-effort and must not undo or block it.
		log.Error().Err(err).Str("observation_id", obs.ID).Str("action", string(event.Action)).
			Msg("reconcile: mirror auto-remediation action to audit log")
	}
}

// observationScope scopes repository calls to the observation's tenant and
// mode, matching how the transaction was written.
func observationScope(ctx context.Context, obs *TransientObservation) context.Context {
	if obs.TenantID != "" {
		ctx = tenant.WithID(ctx, obs.TenantID)
	}
	if obs.Mode != "" {
		ctx = tenant.WithMode(ctx, obs.Mode)
	}
	return ctx
}
