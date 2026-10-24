package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/Lumen-Nexora/Nexora/internal/assets"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/protocols/horizon/operations"
)

// ─── Fakes ─────────────────────────────────────────────────────────────

// fakeClock is a manually advanced clock shared by the remediator and store.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memRemediationStore mirrors the postgres store's semantics: one observation
// per transaction, claim-by-interval, and compare-and-swap transitions.
type memRemediationStore struct {
	mu            sync.Mutex
	clock         *fakeClock
	byTx          map[string]*TransientObservation
	events        map[string][]*RemediationEvent
	trackErr      error
	transitionErr error
	claimErr      error
}

func newMemStore(clock *fakeClock) *memRemediationStore {
	return &memRemediationStore{
		clock:  clock,
		byTx:   make(map[string]*TransientObservation),
		events: make(map[string][]*RemediationEvent),
	}
}

func (m *memRemediationStore) TrackTransient(_ context.Context, obs *TransientObservation, event *RemediationEvent) (*TransientObservation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trackErr != nil {
		return nil, false, m.trackErr
	}
	if existing, ok := m.byTx[obs.TransactionID]; ok {
		cp := *existing
		return &cp, false, nil
	}
	cp := *obs
	m.byTx[obs.TransactionID] = &cp
	m.events[obs.ID] = append(m.events[obs.ID], event)
	out := cp
	return &out, true, nil
}

func (m *memRemediationStore) ClaimDueTransients(_ context.Context, minInterval time.Duration, limit int) ([]*TransientObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimErr != nil {
		return nil, m.claimErr
	}
	now := m.clock.Now()
	var claimed []*TransientObservation
	for _, obs := range m.byTx {
		if len(claimed) >= limit {
			break
		}
		if obs.Status != TransientObserving || obs.LastEvaluatedAt.After(now.Add(-minInterval)) {
			continue
		}
		obs.LastEvaluatedAt = now
		cp := *obs
		claimed = append(claimed, &cp)
	}
	return claimed, nil
}

func (m *memRemediationStore) TransitionTransient(_ context.Context, prevHash string, next *TransientObservation, event *RemediationEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.transitionErr != nil {
		return m.transitionErr
	}
	current, ok := m.byTx[next.TransactionID]
	if !ok || current.Status != TransientObserving || current.TraceHash != prevHash {
		return domain.ErrConcurrentUpdate
	}
	cp := *next
	m.byTx[next.TransactionID] = &cp
	m.events[next.ID] = append(m.events[next.ID], event)
	return nil
}

func (m *memRemediationStore) get(txID string) *TransientObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	obs, ok := m.byTx[txID]
	if !ok {
		return nil
	}
	cp := *obs
	return &cp
}

func (m *memRemediationStore) eventsFor(txID string) []*RemediationEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	obs, ok := m.byTx[txID]
	if !ok {
		return nil
	}
	return append([]*RemediationEvent(nil), m.events[obs.ID]...)
}

func (m *memRemediationStore) setDetectedAt(txID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byTx[txID].DetectedAt = at
}

type fakeTxLoader struct {
	mu  sync.Mutex
	txs map[string]*domain.Transaction
	err error
}

func (f *fakeTxLoader) GetByID(_ context.Context, id string) (*domain.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	tx, ok := f.txs[id]
	if !ok {
		return nil, domain.ErrTransactionNotFound
	}
	cp := *tx
	return &cp, nil
}

func (f *fakeTxLoader) setStatus(id string, status domain.TransactionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs[id].Status = status
}

type fakeLedgerSource struct {
	mu     sync.Mutex
	ledger int64
	err    error
}

func (f *fakeLedgerSource) LatestLedger(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ledger, f.err
}

func (f *fakeLedgerSource) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ledger++
}

// horizonView is a concurrency-safe Horizon stand-in whose answer for the
// test hash can be flipped between "missing", "visible", and "erroring".
type horizonView struct {
	mockStellarClient
	mu        sync.Mutex
	detail    horizon.Transaction
	detailErr error
	ops       []operations.Operation
	calls     int
}

func (h *horizonView) TransactionDetail(_ string) (horizon.Transaction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.detail, h.detailErr
}

func (h *horizonView) OperationsForTransaction(_ string) ([]operations.Operation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ops, nil
}

func (h *horizonView) setMissing() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.detail = horizon.Transaction{}
	h.detailErr = &horizonclient.Error{Response: &http.Response{StatusCode: http.StatusNotFound}}
}

func (h *horizonView) setUnavailable() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.detail = horizon.Transaction{}
	h.detailErr = &horizonclient.Error{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}}
}

func (h *horizonView) setVisible(ledger int32, ops []operations.Operation) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.detail = horizon.Transaction{Successful: true, Hash: "hash-1", Ledger: ledger}
	h.detailErr = nil
	h.ops = ops
}

// statusRepo records the reconciliation side effects the remediator must (or
// must not) trigger.
type statusRepo struct {
	mockRepo
	mu            sync.Mutex
	statusUpdates []domain.TransactionStatus
	reconciledAt  int
	statusErr     error
}

func (r *statusRepo) UpdateReconciliationStatus(_ context.Context, _ string, status domain.TransactionStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.statusErr != nil {
		return r.statusErr
	}
	r.statusUpdates = append(r.statusUpdates, status)
	return nil
}

func (r *statusRepo) UpdateReconciledAt(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconciledAt++
	return nil
}

func (r *statusRepo) WriteAuditLog(ctx context.Context, entry *domain.AuditLogEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mockRepo.WriteAuditLog(ctx, entry)
}

func (r *statusRepo) snapshot() (statuses []domain.TransactionStatus, reconciled int, audits []*domain.AuditLogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.TransactionStatus(nil), r.statusUpdates...), r.reconciledAt,
		append([]*domain.AuditLogEntry(nil), r.auditLogs...)
}

type recordingAuditTrail struct {
	mu     sync.Mutex
	events []*domain.AuditEvent
	err    error
}

func (a *recordingAuditTrail) Record(_ context.Context, event *domain.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	return a.err
}

func (a *recordingAuditTrail) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, e := range a.events {
		out = append(out, e.Action)
	}
	return out
}

// ─── Harness ───────────────────────────────────────────────────────────

type remediationHarness struct {
	svc     *Service
	clock   *fakeClock
	store   *memRemediationStore
	loader  *fakeTxLoader
	ledgers *fakeLedgerSource
	horizon *horizonView
	repo    *statusRepo
	audit   *recordingAuditTrail
	tx      *domain.Transaction
	cfg     AutoRemediationConfig
}

func newRemediationHarness(t *testing.T, txAge time.Duration) *remediationHarness {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	tenantID := "11111111-1111-1111-1111-111111111111"
	tx := &domain.Transaction{
		ID:        "tx-1",
		TxHash:    "hash-1",
		Status:    domain.StatusConfirmed,
		Asset:     "USDC",
		Amount:    decimal.RequireFromString("100"),
		Fee:       decimal.Zero,
		TenantID:  &tenantID,
		Mode:      domain.ModeLive,
		CreatedAt: clock.Now().Add(-txAge),
	}
	cfg := AutoRemediationConfig{
		TransientWindow:    10 * time.Minute,
		MaxCycles:          3,
		MaxAge:             15 * time.Minute,
		EvaluationInterval: 30 * time.Second,
	}
	h := &remediationHarness{
		clock:   clock,
		store:   newMemStore(clock),
		loader:  &fakeTxLoader{txs: map[string]*domain.Transaction{tx.ID: tx}},
		ledgers: &fakeLedgerSource{ledger: 1000},
		horizon: &horizonView{},
		repo:    &statusRepo{},
		audit:   &recordingAuditTrail{},
		tx:      tx,
		cfg:     cfg,
	}
	h.horizon.setMissing()
	remediator := NewAutoRemediator(cfg, h.store, h.loader).
		WithLedgerSource(h.ledgers).
		WithAuditTrail(h.audit)
	remediator.now = clock.Now
	h.svc = (&Service{
		repo:          h.repo,
		stellar:       h.horizon,
		alerting:      alerting.NewClient("", "test"),
		assetRegistry: assets.NewRegistry(realIssuer, ""),
	}).WithAutoRemediator(remediator)
	return h
}

func (h *remediationHarness) matchingOps(t *testing.T) []operations.Operation {
	t.Helper()
	return []operations.Operation{
		mustPaymentOp(t, "payment", platformFrom, platformTo, "100.0000000", "credit_alphanum4", "USDC", realIssuer),
	}
}

// reconcileOnce runs the confirmed-transaction check as the scheduled pass
// would, on a fresh copy of the transaction.
func (h *remediationHarness) reconcileOnce(t *testing.T) {
	t.Helper()
	cp := *h.tx
	if err := h.svc.checkTransaction(context.Background(), &cp); err != nil {
		t.Fatalf("checkTransaction: %v", err)
	}
}

// nextCycle closes a ledger, moves past the evaluation interval, and runs one
// auto-remediation pass.
func (h *remediationHarness) nextCycle(t *testing.T) AutoRemediationResult {
	t.Helper()
	h.ledgers.close()
	h.clock.Advance(time.Minute)
	result, err := h.svc.RunAutoRemediation(context.Background())
	if err != nil {
		t.Fatalf("RunAutoRemediation: %v", err)
	}
	return result
}

func (h *remediationHarness) assertTraceVerifies(t *testing.T) {
	t.Helper()
	events := h.store.eventsFor(h.tx.ID)
	if len(events) == 0 {
		t.Fatal("expected a remediation trace")
	}
	if err := VerifyRemediationTrace(events); err != nil {
		t.Fatalf("remediation trace does not verify: %v", err)
	}
	if obs := h.store.get(h.tx.ID); obs.TraceHash != events[len(events)-1].Hash {
		t.Fatalf("observation trace head %s does not match last event %s", obs.TraceHash, events[len(events)-1].Hash)
	}
}

func countStatus(statuses []domain.TransactionStatus, want domain.TransactionStatus) int {
	n := 0
	for _, s := range statuses {
		if s == want {
			n++
		}
	}
	return n
}

// ─── Transient discrepancy is remediated ───────────────────────────────

func TestAutoRemediation_TransientDiscrepancySelfCorrectsOnNextLedger(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)

	h.reconcileOnce(t)

	statuses, _, audits := h.repo.snapshot()
	if len(statuses) != 0 || len(audits) != 0 {
		t.Fatalf("transient 404 must not be flagged, got statuses=%v audits=%d", statuses, len(audits))
	}
	obs := h.store.get(h.tx.ID)
	if obs == nil || obs.Status != TransientObserving || obs.Cycles != 1 || obs.FirstLedger != 1000 {
		t.Fatalf("expected an observing observation at ledger 1000, got %+v", obs)
	}

	// Horizon ingests the next ledger and now has the transaction.
	h.horizon.setVisible(1001, h.matchingOps(t))
	result := h.nextCycle(t)
	if result.Resolved != 1 || result.Escalated != 0 || result.Failed != 0 {
		t.Fatalf("expected one resolution, got %+v", result)
	}

	statuses, reconciled, audits := h.repo.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("resolution must not change transaction status, got %v", statuses)
	}
	if reconciled != 1 {
		t.Fatalf("expected reconciled_at to be stamped once, got %d", reconciled)
	}
	if len(audits) != 1 || audits[0].Outcome != domain.AuditOK {
		t.Fatalf("expected a single ok ledger audit entry, got %+v", audits)
	}
	if obs := h.store.get(h.tx.ID); obs.Status != TransientResolved || obs.ResolvedAt == nil {
		t.Fatalf("expected resolved observation, got %+v", obs)
	}
	h.assertTraceVerifies(t)

	actions := h.audit.actions()
	want := []string{"reconciliation.auto_remediation.detected", "reconciliation.auto_remediation.resolved"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	last := h.audit.events[len(h.audit.events)-1]
	if last.Metadata["trace_hash"] != h.store.get(h.tx.ID).TraceHash || last.ResourceID != h.tx.ID || last.TenantID != *h.tx.TenantID {
		t.Fatalf("audit event does not carry the trace: %+v", last)
	}
}

func TestAutoRemediation_ResolvesAfterSeveralMissingCycles(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)

	for i := 0; i < 2; i++ {
		if result := h.nextCycle(t); result.Observing != 1 {
			t.Fatalf("cycle %d: expected still observing, got %+v", i+1, result)
		}
	}
	if obs := h.store.get(h.tx.ID); obs.Cycles != 3 {
		t.Fatalf("expected 3 cycles counted, got %d", obs.Cycles)
	}

	h.horizon.setVisible(1003, h.matchingOps(t))
	if result := h.nextCycle(t); result.Resolved != 1 {
		t.Fatalf("expected resolution on the third re-check, got %+v", result)
	}
	statuses, _, _ := h.repo.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("expected no status change, got %v", statuses)
	}
	h.assertTraceVerifies(t)
}

// ─── Non-transient states are left alone and escalated ─────────────────

func TestAutoRemediation_OutsideTransientWindowIsFlaggedImmediately(t *testing.T) {
	h := newRemediationHarness(t, 10*time.Minute+time.Nanosecond)

	h.reconcileOnce(t)

	statuses, _, audits := h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 1 {
		t.Fatalf("expected immediate reconciliation_failed, got %v", statuses)
	}
	if len(audits) != 1 || audits[0].Outcome != domain.AuditNotFound {
		t.Fatalf("expected a not_found audit entry, got %+v", audits)
	}
	if obs := h.store.get(h.tx.ID); obs != nil {
		t.Fatalf("non-transient discrepancy must not be tracked, got %+v", obs)
	}
}

func TestAutoRemediation_MismatchIsNeverDeferred(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	// Horizon has the transaction, but it paid the wrong destination.
	h.horizon.setVisible(1000, []operations.Operation{
		mustPaymentOp(t, "payment", platformFrom, attackerAddr, "100.0000000", "credit_alphanum4", "USDC", fakeIssuer),
	})

	h.reconcileOnce(t)

	statuses, reconciled, audits := h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 1 || reconciled != 0 {
		t.Fatalf("expected mismatch flagged, got statuses=%v reconciled=%d", statuses, reconciled)
	}
	if len(audits) != 1 || audits[0].Outcome != domain.AuditMismatch {
		t.Fatalf("expected mismatch audit, got %+v", audits)
	}
	if h.store.get(h.tx.ID) != nil {
		t.Fatal("a Horizon-confirmed mismatch is not transient and must not be tracked")
	}
}

func TestAutoRemediation_UnsuccessfulOnHorizonIsNeverDeferred(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.horizon.mu.Lock()
	h.horizon.detail = horizon.Transaction{Successful: false, Hash: "hash-1", Ledger: 1000}
	h.horizon.detailErr = nil
	h.horizon.mu.Unlock()

	h.reconcileOnce(t)

	statuses, _, _ := h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 1 {
		t.Fatalf("expected unsuccessful tx flagged, got %v", statuses)
	}
	if h.store.get(h.tx.ID) != nil {
		t.Fatal("unsuccessful transaction must not be tracked")
	}
}

func TestAutoRemediation_PersistentDiscrepancyEscalatesAfterMaxCycles(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)

	// Detection was cycle 1; re-checks 1 and 2 bring it to MaxCycles (3).
	for i := 0; i < 2; i++ {
		if result := h.nextCycle(t); result.Observing != 1 {
			t.Fatalf("re-check %d: expected observing, got %+v", i+1, result)
		}
		statuses, _, _ := h.repo.snapshot()
		if len(statuses) != 0 {
			t.Fatalf("re-check %d: must not flag before max cycles, got %v", i+1, statuses)
		}
	}

	// Fourth consecutive cycle with the discrepancy: persisted beyond 3.
	if result := h.nextCycle(t); result.Escalated != 1 {
		t.Fatalf("expected escalation, got %+v", result)
	}
	statuses, reconciled, audits := h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 1 || reconciled != 0 {
		t.Fatalf("expected one reconciliation_failed flag, got statuses=%v reconciled=%d", statuses, reconciled)
	}
	if len(audits) != 1 || audits[0].Outcome != domain.AuditNotFound {
		t.Fatalf("expected not_found audit on escalation, got %+v", audits)
	}
	obs := h.store.get(h.tx.ID)
	if obs.Status != TransientEscalated {
		t.Fatalf("expected escalated observation, got %+v", obs)
	}
	h.assertTraceVerifies(t)

	// Escalated observations are final: further passes do nothing and a
	// later reconcile pass flags normally instead of deferring again.
	if result := h.nextCycle(t); result.Evaluated != 0 {
		t.Fatalf("escalated observation must not be re-evaluated, got %+v", result)
	}
	h.reconcileOnce(t)
	statuses, _, _ = h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 2 {
		t.Fatalf("expected the re-reconciled tx to be flagged directly, got %v", statuses)
	}
}

func TestAutoRemediation_StateChangeDuringObservationEscalatesWithoutFlagging(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)

	h.loader.setStatus(h.tx.ID, domain.StatusReconciliationFailed)
	h.horizon.setVisible(1001, h.matchingOps(t))
	if result := h.nextCycle(t); result.Escalated != 1 {
		t.Fatalf("expected escalation, got %+v", result)
	}
	statuses, reconciled, audits := h.repo.snapshot()
	if len(statuses) != 0 || reconciled != 0 || len(audits) != 0 {
		t.Fatalf("must not touch a transaction someone else moved: statuses=%v reconciled=%d audits=%d",
			statuses, reconciled, len(audits))
	}
	if h.horizon.calls != 1 {
		t.Fatalf("expected no Horizon re-check for a moved transaction, got %d calls", h.horizon.calls)
	}
}

// ─── Idempotency and concurrency ───────────────────────────────────────

func TestAutoRemediation_RepeatedDetectionIsIdempotent(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)

	h.reconcileOnce(t)
	h.reconcileOnce(t)

	if events := h.store.eventsFor(h.tx.ID); len(events) != 1 {
		t.Fatalf("expected a single detection event, got %d", len(events))
	}
	if got := h.audit.actions(); len(got) != 1 {
		t.Fatalf("expected a single audit action, got %v", got)
	}
	statuses, _, _ := h.repo.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("expected no flag on re-detection, got %v", statuses)
	}
}

func TestAutoRemediation_RunningTwiceIsIdempotent(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	h.horizon.setVisible(1001, h.matchingOps(t))

	first := h.nextCycle(t)
	second := h.nextCycle(t)
	if first.Resolved != 1 || second.Evaluated != 0 {
		t.Fatalf("expected one resolution then nothing, got %+v then %+v", first, second)
	}

	// Without the interval elapsing, an immediate re-run claims nothing.
	h2 := newRemediationHarness(t, time.Minute)
	h2.reconcileOnce(t)
	h2.ledgers.close()
	h2.clock.Advance(time.Minute)
	if _, err := h2.svc.RunAutoRemediation(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := h2.svc.RunAutoRemediation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Evaluated != 0 {
		t.Fatalf("expected an immediate re-run to claim nothing, got %+v", result)
	}
	if obs := h2.store.get(h2.tx.ID); obs.Cycles != 2 {
		t.Fatalf("expected exactly one extra cycle, got %d", obs.Cycles)
	}

	_, reconciled, audits := h.repo.snapshot()
	if reconciled != 1 || len(audits) != 1 {
		t.Fatalf("expected resolution side effects exactly once, got reconciled=%d audits=%d", reconciled, len(audits))
	}
}

func TestAutoRemediation_ConcurrentRunsResolveOnce(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	h.horizon.setVisible(1001, h.matchingOps(t))
	h.ledgers.close()
	h.clock.Advance(time.Minute)

	var wg sync.WaitGroup
	results := make([]AutoRemediationResult, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.svc.RunAutoRemediation(context.Background())
			if err != nil {
				t.Errorf("run %d: %v", i, err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	resolved := 0
	for _, r := range results {
		resolved += r.Resolved
	}
	if resolved != 1 {
		t.Fatalf("expected exactly one resolution across concurrent runs, got %d", resolved)
	}
	if events := h.store.eventsFor(h.tx.ID); len(events) != 2 {
		t.Fatalf("expected detected+resolved events only, got %d", len(events))
	}
	h.assertTraceVerifies(t)
}

func TestAutoRemediation_LostTransitionRaceDoesNotFlag(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	for i := 0; i < 2; i++ {
		h.nextCycle(t)
	}

	// Another reconciler closed the observation first.
	h.store.transitionErr = domain.ErrConcurrentUpdate
	result := h.nextCycle(t)
	if result.Escalated != 0 || result.Failed != 0 {
		t.Fatalf("a lost race is neither an escalation nor a failure, got %+v", result)
	}
	statuses, _, _ := h.repo.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("the losing reconciler must not flag, got %v", statuses)
	}
}

// ─── Failures during remediation ───────────────────────────────────────

func TestAutoRemediation_StoreFailureFallsBackToImmediateFlag(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.store.trackErr = errors.New("db down")

	h.reconcileOnce(t)

	statuses, _, _ := h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 1 {
		t.Fatalf("expected fallback flag when tracking fails, got %v", statuses)
	}
}

func TestAutoRemediation_HorizonErrorLeavesObservationForRetry(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	before := h.store.get(h.tx.ID)

	h.horizon.setUnavailable()
	result := h.nextCycle(t)
	if result.Failed != 1 || result.Escalated != 0 {
		t.Fatalf("expected a retryable failure, got %+v", result)
	}
	after := h.store.get(h.tx.ID)
	if after.Status != TransientObserving || after.Cycles != before.Cycles || after.TraceHash != before.TraceHash {
		t.Fatalf("a failed evaluation must not advance the observation: before=%+v after=%+v", before, after)
	}

	h.horizon.setVisible(1002, h.matchingOps(t))
	if result := h.nextCycle(t); result.Resolved != 1 {
		t.Fatalf("expected resolution once Horizon recovers, got %+v", result)
	}
	h.assertTraceVerifies(t)
}

func TestAutoRemediation_TransitionFailureIsRetriedSafely(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	h.horizon.setVisible(1001, h.matchingOps(t))

	h.store.transitionErr = errors.New("db write failed")
	if result := h.nextCycle(t); result.Failed != 1 || result.Resolved != 0 {
		t.Fatalf("expected failed resolution, got %+v", result)
	}
	if obs := h.store.get(h.tx.ID); obs.Status != TransientObserving {
		t.Fatalf("observation must stay observing after a failed write, got %s", obs.Status)
	}

	h.store.transitionErr = nil
	if result := h.nextCycle(t); result.Resolved != 1 {
		t.Fatalf("expected resolution on retry, got %+v", result)
	}
	statuses, _, audits := h.repo.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("retry must not flag a verified transaction, got %v", statuses)
	}
	for _, a := range audits {
		if a.Outcome != domain.AuditOK {
			t.Fatalf("retry produced a non-ok audit entry: %+v", a)
		}
	}
	h.assertTraceVerifies(t)
}

func TestAutoRemediation_FlagFailureOnEscalationIsReported(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	for i := 0; i < 2; i++ {
		h.nextCycle(t)
	}

	h.repo.statusErr = errors.New("db write failed")
	result := h.nextCycle(t)
	if result.Failed != 1 {
		t.Fatalf("expected the failed flag to be reported, got %+v", result)
	}
	if obs := h.store.get(h.tx.ID); obs.Status != TransientEscalated {
		t.Fatalf("expected the observation to be escalated regardless, got %s", obs.Status)
	}
}

func TestAutoRemediation_AuditMirrorFailureDoesNotBlockRemediation(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.audit.err = errors.New("audit store down")
	h.reconcileOnce(t)
	h.horizon.setVisible(1001, h.matchingOps(t))

	if result := h.nextCycle(t); result.Resolved != 1 {
		t.Fatalf("expected resolution despite audit mirror failure, got %+v", result)
	}
	h.assertTraceVerifies(t)
}

func TestAutoRemediation_ClaimFailureIsReturned(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.store.claimErr = errors.New("db down")
	if _, err := h.svc.RunAutoRemediation(context.Background()); err == nil {
		t.Fatal("expected claim failure to be returned")
	}
}

// ─── Threshold boundaries ──────────────────────────────────────────────

func TestAutoRemediation_TransientWindowBoundary(t *testing.T) {
	cases := []struct {
		name    string
		age     time.Duration
		tracked bool
	}{
		{"just created", 0, true},
		{"exactly at window", 10 * time.Minute, true},
		{"one nanosecond past window", 10*time.Minute + time.Nanosecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRemediationHarness(t, tc.age)
			h.reconcileOnce(t)
			tracked := h.store.get(h.tx.ID) != nil
			if tracked != tc.tracked {
				t.Fatalf("tracked = %v, want %v", tracked, tc.tracked)
			}
			statuses, _, _ := h.repo.snapshot()
			if flagged := len(statuses) > 0; flagged == tc.tracked {
				t.Fatalf("flagged = %v, want %v", flagged, !tc.tracked)
			}
		})
	}
}

func TestAutoRemediation_OnlyConfirmedTransactionsAreCandidates(t *testing.T) {
	r := NewAutoRemediator(AutoRemediationConfig{}, newMemStore(&fakeClock{}), &fakeTxLoader{})
	now := time.Now().UTC()
	base := domain.Transaction{ID: "tx", TxHash: "h", Status: domain.StatusConfirmed, CreatedAt: now}
	if !r.isTransientCandidate(&base, now) {
		t.Fatal("fresh confirmed transaction should be a candidate")
	}
	for _, status := range []domain.TransactionStatus{domain.StatusPending, domain.StatusSubmitted, domain.StatusFailed, domain.StatusReconciliationFailed, domain.StatusComplianceHold} {
		tx := base
		tx.Status = status
		if r.isTransientCandidate(&tx, now) {
			t.Fatalf("status %s must not be a candidate", status)
		}
	}
	noHash := base
	noHash.TxHash = ""
	if r.isTransientCandidate(&noHash, now) {
		t.Fatal("a transaction without a hash must not be a candidate")
	}
	if r.isTransientCandidate(nil, now) {
		t.Fatal("nil transaction must not be a candidate")
	}
}

func TestAutoRemediation_NoLedgerCloseMeansNoCycle(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)

	// Time passes but Horizon has not ingested a new ledger.
	for i := 0; i < 5; i++ {
		h.clock.Advance(time.Minute)
		result, err := h.svc.RunAutoRemediation(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Observing != 1 {
			t.Fatalf("expected waiting observation, got %+v", result)
		}
	}
	if obs := h.store.get(h.tx.ID); obs.Cycles != 1 {
		t.Fatalf("no ledger closed, so no cycle should be counted, got %d", obs.Cycles)
	}
	if h.horizon.calls != 1 {
		t.Fatalf("Horizon should not be re-queried until a ledger closes, got %d calls", h.horizon.calls)
	}
}

func TestAutoRemediation_MaxAgeBoundary(t *testing.T) {
	t.Run("at max age keeps observing", func(t *testing.T) {
		h := newRemediationHarness(t, time.Minute)
		h.reconcileOnce(t)
		h.clock.Advance(time.Minute)
		h.store.setDetectedAt(h.tx.ID, h.clock.Now().Add(-h.cfg.MaxAge))
		result, err := h.svc.RunAutoRemediation(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Observing != 1 || result.Escalated != 0 {
			t.Fatalf("expected still observing at exactly max age, got %+v", result)
		}
	})
	t.Run("past max age escalates without a ledger close", func(t *testing.T) {
		h := newRemediationHarness(t, time.Minute)
		h.reconcileOnce(t)
		h.clock.Advance(time.Minute)
		h.store.setDetectedAt(h.tx.ID, h.clock.Now().Add(-h.cfg.MaxAge-time.Nanosecond))
		result, err := h.svc.RunAutoRemediation(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Escalated != 1 {
			t.Fatalf("expected escalation past max age, got %+v", result)
		}
		statuses, _, _ := h.repo.snapshot()
		if countStatus(statuses, domain.StatusReconciliationFailed) != 1 {
			t.Fatalf("expected flag on max-age escalation, got %v", statuses)
		}
	})
	t.Run("past max age still resolves if Horizon verifies", func(t *testing.T) {
		h := newRemediationHarness(t, time.Minute)
		h.reconcileOnce(t)
		h.clock.Advance(time.Minute)
		h.store.setDetectedAt(h.tx.ID, h.clock.Now().Add(-h.cfg.MaxAge-time.Second))
		h.horizon.setVisible(1000, h.matchingOps(t))
		result, err := h.svc.RunAutoRemediation(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Resolved != 1 {
			t.Fatalf("a fully verified transaction should resolve, got %+v", result)
		}
	})
}

func TestAutoRemediation_MaxCyclesIsConfigurable(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	remediator := NewAutoRemediator(AutoRemediationConfig{MaxCycles: 1}, h.store, h.loader).WithLedgerSource(h.ledgers)
	remediator.now = h.clock.Now
	h.svc.WithAutoRemediator(remediator)
	h.reconcileOnce(t)

	if result := h.nextCycle(t); result.Escalated != 1 {
		t.Fatalf("with MaxCycles=1 the first re-check should escalate, got %+v", result)
	}
}

func TestAutoRemediation_DisabledFlagsImmediately(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.svc.WithAutoRemediator(nil)
	h.reconcileOnce(t)
	statuses, _, _ := h.repo.snapshot()
	if countStatus(statuses, domain.StatusReconciliationFailed) != 1 {
		t.Fatalf("without a remediator a 404 is flagged immediately, got %v", statuses)
	}
	if result, err := h.svc.RunAutoRemediation(context.Background()); err != nil || result.Evaluated != 0 {
		t.Fatalf("disabled remediator should be a no-op, got %+v %v", result, err)
	}
}

func TestAutoRemediationConfig_Defaults(t *testing.T) {
	cfg := AutoRemediationConfig{MaxCycles: -1, TransientWindow: -time.Second}.withDefaults()
	if cfg.TransientWindow != DefaultTransientWindow || cfg.MaxCycles != DefaultRemediationMaxCycles ||
		cfg.MaxAge != DefaultRemediationMaxAge || cfg.EvaluationInterval != DefaultRemediationEvaluationInterval ||
		cfg.BatchSize != defaultRemediationBatchSize {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

// ─── Cryptographic trace ───────────────────────────────────────────────

func TestVerifyRemediationTrace_DetectsTampering(t *testing.T) {
	h := newRemediationHarness(t, time.Minute)
	h.reconcileOnce(t)
	h.nextCycle(t)
	h.horizon.setVisible(1002, h.matchingOps(t))
	h.nextCycle(t)

	events := h.store.eventsFor(h.tx.ID)
	if len(events) != 3 {
		t.Fatalf("expected detected, still_missing, resolved events, got %d", len(events))
	}
	if err := VerifyRemediationTrace(events); err != nil {
		t.Fatalf("untampered trace should verify: %v", err)
	}

	tampered := make([]*RemediationEvent, len(events))
	for i, e := range events {
		cp := *e
		tampered[i] = &cp
	}
	tampered[1].Reason = "edited after the fact"
	if err := VerifyRemediationTrace(tampered); err == nil {
		t.Fatal("expected an edited event to fail verification")
	}
	if err := VerifyRemediationTrace([]*RemediationEvent{events[0], events[2]}); err == nil {
		t.Fatal("expected a removed event to fail verification")
	}
}

// ─── Horizon ledger source ─────────────────────────────────────────────

func TestHorizonLedgerSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok/":
			_, _ = w.Write([]byte(`{"history_latest_ledger": 52012345, "core_latest_ledger": 52012346}`))
		case "/empty/":
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()

	ledger, err := NewHorizonLedgerSource(srv.URL+"/ok/", time.Second).LatestLedger(context.Background())
	if err != nil || ledger != 52012345 {
		t.Fatalf("LatestLedger = %d, %v", ledger, err)
	}
	if _, err := NewHorizonLedgerSource(srv.URL+"/empty", time.Second).LatestLedger(context.Background()); err == nil {
		t.Fatal("expected an error when Horizon reports no ledger")
	}
	if _, err := NewHorizonLedgerSource(srv.URL+"/down", 0).LatestLedger(context.Background()); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}
