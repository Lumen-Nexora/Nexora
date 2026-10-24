package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

type fakeInvariantRepo struct {
	totals     []InvariantTotals
	configured bool
	err        error
	calls      int
}

func (f *fakeInvariantRepo) GetInvariantTotals(context.Context) ([]InvariantTotals, bool, error) {
	f.calls++
	return f.totals, f.configured, f.err
}

// fakeHalter mirrors the sweep_halts semantics: one idempotent row per source.
type fakeHalter struct {
	halts     map[string]string
	haltErr   error
	resumeErr error
	haltCalls int
}

func newFakeHalter() *fakeHalter { return &fakeHalter{halts: map[string]string{}} }

func (f *fakeHalter) HaltSweeps(_ context.Context, source, reason string) (bool, error) {
	f.haltCalls++
	if f.haltErr != nil {
		return false, f.haltErr
	}
	if _, ok := f.halts[source]; ok {
		return false, nil
	}
	f.halts[source] = reason
	return true, nil
}

func (f *fakeHalter) ResumeSweeps(_ context.Context, source string) (bool, error) {
	if f.resumeErr != nil {
		return false, f.resumeErr
	}
	_, ok := f.halts[source]
	delete(f.halts, source)
	return ok, nil
}

type alertRecorder struct {
	mu     sync.Mutex
	alerts []alerting.Alert
}

func (r *alertRecorder) byLevel(level alerting.Level) []alerting.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []alerting.Alert
	for _, a := range r.alerts {
		if a.Level == level {
			out = append(out, a)
		}
	}
	return out
}

func newAlertRecorder(t *testing.T) (*alertRecorder, *alerting.Client) {
	t.Helper()
	rec := &alertRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a alerting.Alert
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			t.Errorf("decode alert: %v", err)
		}
		rec.mu.Lock()
		rec.alerts = append(rec.alerts, a)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rec, alerting.NewClient(srv.URL, "test")
}

func balancedTotals() []InvariantTotals {
	return []InvariantTotals{
		{Asset: "XLM", TenantBalances: dec("1500.0000000"), AccumulatedFees: dec("4.5000000"), PooledReserves: dec("1504.5000000")},
		{Asset: "USDC", TenantBalances: dec("250.1234567"), AccumulatedFees: dec("0.7500000"), PooledReserves: dec("250.8734567")},
	}
}

func TestEvaluateInvariantBalancedBooks(t *testing.T) {
	results := EvaluateInvariant(balancedTotals())
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Asset != "USDC" || results[1].Asset != "XLM" {
		t.Fatalf("results not sorted by asset: %+v", results)
	}
	for _, r := range results {
		if !r.Balanced || !r.Difference.IsZero() {
			t.Fatalf("expected %s balanced, got %+v", r.Asset, r)
		}
		if !r.Expected.Equal(r.PooledReserves) {
			t.Fatalf("expected tenants+fees == pooled for %s, got %s vs %s", r.Asset, r.Expected, r.PooledReserves)
		}
	}
}

func TestEvaluateInvariantDetectsSmallestImbalance(t *testing.T) {
	// One stroop (the smallest NUMERIC(20,7) unit) missing from the pool.
	results := EvaluateInvariant([]InvariantTotals{
		{Asset: "XLM", TenantBalances: dec("100.0000000"), AccumulatedFees: dec("0.3000000"), PooledReserves: dec("100.2999999")},
	})
	if results[0].Balanced {
		t.Fatal("a one-stroop shortfall must be a violation")
	}
	if !results[0].Difference.Equal(dec("-0.0000001")) {
		t.Fatalf("difference = %s, want -0.0000001", results[0].Difference)
	}
}

func TestEvaluateInvariantLargeValuesAreExact(t *testing.T) {
	// Far beyond float64's 15-16 significant digits: a float comparison
	// would round both sides to the same value and miss the corruption.
	huge := "9999999999999.9999999"
	results := EvaluateInvariant([]InvariantTotals{
		{Asset: "BIG", TenantBalances: dec(huge), AccumulatedFees: dec("0.0000001"), PooledReserves: dec("10000000000000.0000000")},
		{Asset: "BAD", TenantBalances: dec(huge), AccumulatedFees: dec("0.0000001"), PooledReserves: dec("10000000000000.0000001")},
	})
	if results[0].Asset != "BAD" || results[0].Balanced {
		t.Fatalf("expected BAD to be a violation, got %+v", results[0])
	}
	if !results[0].Difference.Equal(dec("0.0000001")) {
		t.Fatalf("difference = %s, want 0.0000001", results[0].Difference)
	}
	if !results[1].Balanced {
		t.Fatalf("expected BIG to balance exactly, got %+v", results[1])
	}
}

func TestEvaluateInvariantEmptyAndZero(t *testing.T) {
	if got := EvaluateInvariant(nil); len(got) != 0 {
		t.Fatalf("expected no results for no data, got %+v", got)
	}
	got := EvaluateInvariant([]InvariantTotals{{Asset: "XLM"}})
	if !got[0].Balanced {
		t.Fatalf("all-zero totals must balance, got %+v", got[0])
	}
}

func TestInvariantCheckerBalancedBooksPass(t *testing.T) {
	rec, client := newAlertRecorder(t)
	halter := newFakeHalter()
	checker := NewInvariantChecker(&fakeInvariantRepo{totals: balancedTotals(), configured: true}, halter, client)

	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.Configured || !report.Balanced() || len(report.Assets) != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.SweepsHalted || halter.haltCalls != 0 || len(halter.halts) != 0 {
		t.Fatalf("balanced books must not halt sweeps: %+v", halter)
	}
	if len(rec.alerts) != 0 {
		t.Fatalf("balanced books must not alert, got %+v", rec.alerts)
	}
}

func TestInvariantCheckerCorruptionAlertsAndHaltsSweeps(t *testing.T) {
	rec, client := newAlertRecorder(t)
	halter := newFakeHalter()
	totals := balancedTotals()
	// Simulated ledger corruption: a tenant balance was inflated by 10 USDC
	// without the pooled reserve receiving the funds.
	totals[1].TenantBalances = totals[1].TenantBalances.Add(dec("10"))
	repo := &fakeInvariantRepo{totals: totals, configured: true}
	checker := NewInvariantChecker(repo, halter, client)

	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Balanced() || len(report.Violations) != 1 || report.Violations[0].Asset != "USDC" {
		t.Fatalf("expected exactly one USDC violation, got %+v", report.Violations)
	}
	if !report.Violations[0].Difference.Equal(dec("-10")) {
		t.Fatalf("difference = %s, want -10", report.Violations[0].Difference)
	}
	if !report.SweepsHalted {
		t.Fatal("expected sweeps to be halted")
	}
	if reason, ok := halter.halts[InvariantHaltSource]; !ok || !strings.Contains(reason, "USDC") {
		t.Fatalf("expected a %s halt naming USDC, got %+v", InvariantHaltSource, halter.halts)
	}
	critical := rec.byLevel(alerting.LevelCritical)
	if len(critical) != 1 {
		t.Fatalf("expected one critical alert, got %+v", rec.alerts)
	}
	if !strings.Contains(critical[0].Message, "USDC") || !strings.Contains(critical[0].Message, "difference=-10") ||
		strings.Contains(critical[0].Message, "XLM:") {
		t.Fatalf("alert should describe only the USDC violation: %q", critical[0].Message)
	}

	// The same corruption on the next ticks must keep the halt but not
	// page again.
	for i := 0; i < 3; i++ {
		if _, err := checker.Check(context.Background()); err != nil {
			t.Fatalf("Check #%d: %v", i+2, err)
		}
	}
	if got := len(rec.byLevel(alerting.LevelCritical)); got != 1 {
		t.Fatalf("an ongoing incident must page once, got %d critical alerts", got)
	}
	if len(halter.halts) != 1 {
		t.Fatalf("halts must not stack, got %+v", halter.halts)
	}

	// Books repaired: the checker lifts its own halt and reports recovery.
	repo.totals = balancedTotals()
	report, err = checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check after repair: %v", err)
	}
	if !report.Balanced() || report.SweepsHalted {
		t.Fatalf("expected balanced report with sweeps resumed, got %+v", report)
	}
	if len(halter.halts) != 0 {
		t.Fatalf("expected the invariant halt to be lifted, got %+v", halter.halts)
	}
	if got := len(rec.byLevel(alerting.LevelInfo)); got != 1 {
		t.Fatalf("expected one recovery alert, got %d", got)
	}
}

func TestInvariantCheckerLeavesOtherHaltsAlone(t *testing.T) {
	halter := newFakeHalter()
	halter.halts["operator"] = "manual freeze"
	checker := NewInvariantChecker(&fakeInvariantRepo{totals: balancedTotals(), configured: true}, halter, nil)

	if _, err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, ok := halter.halts["operator"]; !ok {
		t.Fatal("the checker must only lift its own halt")
	}
}

func TestInvariantCheckerMultiAssetReportsEveryViolation(t *testing.T) {
	rec, client := newAlertRecorder(t)
	totals := []InvariantTotals{
		{Asset: "XLM", TenantBalances: dec("10"), AccumulatedFees: dec("1"), PooledReserves: dec("12")},
		{Asset: "USDC", TenantBalances: dec("5"), AccumulatedFees: dec("0"), PooledReserves: dec("5")},
		{Asset: "EURC", TenantBalances: dec("7"), AccumulatedFees: dec("1"), PooledReserves: dec("3")},
	}
	checker := NewInvariantChecker(&fakeInvariantRepo{totals: totals, configured: true}, newFakeHalter(), client)

	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 2 || report.Violations[0].Asset != "EURC" || report.Violations[1].Asset != "XLM" {
		t.Fatalf("expected EURC and XLM violations, got %+v", report.Violations)
	}
	critical := rec.byLevel(alerting.LevelCritical)
	if len(critical) != 1 || !strings.Contains(critical[0].Message, "2 asset(s)") {
		t.Fatalf("expected one alert covering both assets, got %+v", critical)
	}
}

func TestInvariantCheckerUnconfiguredDoesNothing(t *testing.T) {
	rec, client := newAlertRecorder(t)
	halter := newFakeHalter()
	checker := NewInvariantChecker(&fakeInvariantRepo{configured: false}, halter, client)

	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Configured || len(report.Assets) != 0 || halter.haltCalls != 0 || len(rec.alerts) != 0 {
		t.Fatalf("unconfigured checker must be inert, got report=%+v halter=%+v alerts=%+v", report, halter, rec.alerts)
	}
}

func TestInvariantCheckerEmptyLedgerBalances(t *testing.T) {
	halter := newFakeHalter()
	checker := NewInvariantChecker(&fakeInvariantRepo{configured: true}, halter, nil)

	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.Configured || !report.Balanced() || halter.haltCalls != 0 {
		t.Fatalf("an empty ledger must balance, got %+v", report)
	}
}

func TestInvariantCheckerReadErrorNeverHalts(t *testing.T) {
	rec, client := newAlertRecorder(t)
	halter := newFakeHalter()
	checker := NewInvariantChecker(&fakeInvariantRepo{err: errors.New("connection refused")}, halter, client)

	if _, err := checker.Check(context.Background()); err == nil {
		t.Fatal("expected the read error to be returned")
	}
	if halter.haltCalls != 0 || len(rec.alerts) != 0 {
		t.Fatal("a read failure is not a violation and must not halt or page")
	}
}

func TestInvariantCheckerPagesEveryRunWhileHaltFails(t *testing.T) {
	rec, client := newAlertRecorder(t)
	halter := newFakeHalter()
	halter.haltErr = errors.New("db down")
	totals := []InvariantTotals{{Asset: "XLM", TenantBalances: dec("1"), PooledReserves: dec("0")}}
	checker := NewInvariantChecker(&fakeInvariantRepo{totals: totals, configured: true}, halter, client)

	for i := 0; i < 2; i++ {
		report, err := checker.Check(context.Background())
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if report.SweepsHalted {
			t.Fatal("SweepsHalted must be false when the halt write failed")
		}
	}
	critical := rec.byLevel(alerting.LevelCritical)
	if len(critical) != 2 {
		t.Fatalf("expected a page on every run while sweeps cannot be halted, got %d", len(critical))
	}
	if !strings.Contains(critical[0].Message, "could NOT be halted") {
		t.Fatalf("alert must say sweeps were not halted: %q", critical[0].Message)
	}
}

func TestInvariantCheckerWithoutHalterPagesOnce(t *testing.T) {
	rec, client := newAlertRecorder(t)
	totals := []InvariantTotals{{Asset: "XLM", TenantBalances: dec("1"), PooledReserves: dec("2")}}
	checker := NewInvariantChecker(&fakeInvariantRepo{totals: totals, configured: true}, nil, client)

	for i := 0; i < 3; i++ {
		if _, err := checker.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	if got := len(rec.byLevel(alerting.LevelCritical)); got != 1 {
		t.Fatalf("expected one page without a halter, got %d", got)
	}
}

func TestInvariantAlertIsBounded(t *testing.T) {
	var violations []InvariantResult
	for i := 0; i < maxInvariantAlertAssets+5; i++ {
		violations = append(violations, InvariantResult{Asset: "A", Difference: dec("1")})
	}
	msg := formatInvariantViolations(violations)
	if got := strings.Count(msg, "\n") + 1; got != maxInvariantAlertAssets+1 {
		t.Fatalf("expected %d lines, got %d", maxInvariantAlertAssets+1, got)
	}
	if !strings.Contains(msg, "and 5 more asset(s)") {
		t.Fatalf("expected an overflow line, got %q", msg)
	}
	if long := truncateHaltReason(strings.Repeat("x", maxHaltReasonLen*2)); len(long) != maxHaltReasonLen {
		t.Fatalf("halt reason not truncated: %d", len(long))
	}
}

func TestHandleLedgerInvariant(t *testing.T) {
	if err := NewWorker(nil).HandleLedgerInvariant(context.Background(), nil); err != nil {
		t.Fatalf("a worker without a checker must be a no-op, got %v", err)
	}

	failing := NewWorker(nil).WithInvariantChecker(
		NewInvariantChecker(&fakeInvariantRepo{err: errors.New("boom")}, newFakeHalter(), nil))
	if err := failing.HandleLedgerInvariant(context.Background(), nil); err == nil {
		t.Fatal("expected the evaluation error to be returned")
	}

	halter := newFakeHalter()
	totals := []InvariantTotals{{Asset: "XLM", TenantBalances: dec("3"), PooledReserves: dec("1")}}
	violating := NewWorker(nil).WithInvariantChecker(
		NewInvariantChecker(&fakeInvariantRepo{totals: totals, configured: true}, halter, nil))
	if err := violating.HandleLedgerInvariant(context.Background(), nil); err != nil {
		t.Fatalf("a violation is not a task failure, got %v", err)
	}
	if _, ok := halter.halts[InvariantHaltSource]; !ok {
		t.Fatal("expected the worker run to halt sweeps")
	}
}
