package treasury_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/treasury"
	"github.com/Lumen-Nexora/Nexora/internal/wallet"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
)

// fakeService is a treasury.Service double used to test Worker.HandleSweep in
// isolation from the real balance/reserve math.
type fakeService struct {
	configs    []*treasury.Config
	sweepable  map[string]decimal.Decimal
	sweepCalls []sweepCall
}

type sweepCall struct {
	asset       string
	amount      decimal.Decimal
	destination string
	triggeredBy string
}

func (f *fakeService) GetBalances(ctx context.Context) ([]treasury.AssetBalance, error) {
	return nil, nil
}
func (f *fakeService) GetReserveRequirement(ctx context.Context) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (f *fakeService) GetReserveBreakdown(ctx context.Context) (*treasury.ReserveBreakdown, error) {
	return &treasury.ReserveBreakdown{}, nil
}
func (f *fakeService) GetSweepableAmount(ctx context.Context, asset string) (decimal.Decimal, error) {
	return f.sweepable[asset], nil
}
func (f *fakeService) ExecuteSweep(ctx context.Context, asset string, amount decimal.Decimal, destination, triggeredBy string) (string, error) {
	f.sweepCalls = append(f.sweepCalls, sweepCall{asset, amount, destination, triggeredBy})
	return "tx-hash", nil
}
func (f *fakeService) GetConfig(ctx context.Context) ([]*treasury.Config, error) {
	return f.configs, nil
}
func (f *fakeService) UpdateConfig(ctx context.Context, cfg *treasury.Config) error { return nil }
func (f *fakeService) ListSweeps(ctx context.Context, limit, offset int) ([]*treasury.SweepLog, error) {
	return nil, nil
}

// TestHandleSweepSweepsAboveThresholdAndSkipsDisabled verifies the daily
// worker: sweeps an asset whose sweepable amount exceeds its threshold,
// still calls ExecuteSweep with amount=0 (a zero-sweep audit record) when
// sweepable is at or below threshold, and skips assets with
// auto_sweep_enabled=false entirely.
func TestHandleSweepSweepsAboveThresholdAndSkipsDisabled(t *testing.T) {
	svc := &fakeService{
		configs: []*treasury.Config{
			{Asset: "XLM", SweepThreshold: decimal.NewFromInt(100), AutoSweepEnabled: true, ColdStorageAddress: "GCOLD1"},
			{Asset: "USDC", SweepThreshold: decimal.NewFromInt(50), AutoSweepEnabled: true, ColdStorageAddress: "GCOLD2"},
			{Asset: "EURC", SweepThreshold: decimal.NewFromInt(0), AutoSweepEnabled: false, ColdStorageAddress: "GCOLD3"},
		},
		sweepable: map[string]decimal.Decimal{
			"XLM":  decimal.NewFromInt(500), // above its 100 threshold -> real sweep
			"USDC": decimal.NewFromInt(10),  // below its 50 threshold -> zero-amount audit sweep
			"EURC": decimal.NewFromInt(999), // enabled=false -> must not be touched at all
		},
	}

	w := treasury.NewWorker(svc)
	if err := w.HandleSweep(context.Background(), &asynq.Task{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(svc.sweepCalls) != 2 {
		t.Fatalf("expected 2 ExecuteSweep calls (XLM + USDC), got %d: %+v", len(svc.sweepCalls), svc.sweepCalls)
	}

	byAsset := map[string]sweepCall{}
	for _, c := range svc.sweepCalls {
		byAsset[c.asset] = c
	}

	if _, ok := byAsset["EURC"]; ok {
		t.Fatal("disabled asset EURC should not have been swept")
	}

	xlm, ok := byAsset["XLM"]
	if !ok || !xlm.amount.Equal(decimal.NewFromInt(500)) || xlm.triggeredBy != treasury.TriggeredByAuto {
		t.Errorf("unexpected XLM sweep call: %+v", xlm)
	}

	usdc, ok := byAsset["USDC"]
	if !ok || !usdc.amount.IsZero() || usdc.triggeredBy != treasury.TriggeredByAuto {
		t.Errorf("expected a zero-amount audit sweep for USDC, got: %+v", usdc)
	}
}

type fakeSweeper struct {
	calls  int
	report wallet.SweepReport
	err    error
}

func (f *fakeSweeper) SweepAll(context.Context) (wallet.SweepReport, error) {
	f.calls++
	return f.report, f.err
}

func TestHandleCustodialSweep_RunsTheSweeper(t *testing.T) {
	sweeper := &fakeSweeper{report: wallet.SweepReport{Targets: 1, WalletsSwept: 3, Transactions: 1}}
	w := treasury.NewWorker(&fakeService{}).WithCustodialSweeper(sweeper)

	if err := w.HandleCustodialSweep(context.Background(), asynq.NewTask("wallet:custodial_sweep", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sweeper.calls != 1 {
		t.Fatalf("sweeper called %d times, want 1", sweeper.calls)
	}
}

func TestHandleCustodialSweep_ReturnsRunFailureForRetry(t *testing.T) {
	sweeper := &fakeSweeper{err: errors.New("database unavailable")}
	w := treasury.NewWorker(&fakeService{}).WithCustodialSweeper(sweeper)

	if err := w.HandleCustodialSweep(context.Background(), asynq.NewTask("wallet:custodial_sweep", nil)); err == nil {
		t.Fatal("expected the run failure to be returned so asynq retries")
	}
}

func TestHandleCustodialSweep_NoSweeperIsANoop(t *testing.T) {
	w := treasury.NewWorker(&fakeService{})
	if err := w.HandleCustodialSweep(context.Background(), asynq.NewTask("wallet:custodial_sweep", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeGuard struct {
	halted bool
	err    error
	calls  int
}

func (f *fakeGuard) SweepsHalted(context.Context) (bool, string, error) {
	f.calls++
	return f.halted, "ledger_invariant: XLM imbalance", f.err
}

func sweepableService() *fakeService {
	return &fakeService{
		configs:   []*treasury.Config{{Asset: "XLM", SweepThreshold: decimal.NewFromInt(1), AutoSweepEnabled: true, ColdStorageAddress: "GCOLD"}},
		sweepable: map[string]decimal.Decimal{"XLM": decimal.NewFromInt(100)},
	}
}

func TestHandleSweep_HaltedByInvariantSkipsAllAssets(t *testing.T) {
	svc := sweepableService()
	guard := &fakeGuard{halted: true}
	w := treasury.NewWorker(svc).WithSweepGuard(guard)

	if err := w.HandleSweep(context.Background(), &asynq.Task{}); err != nil {
		t.Fatalf("a halted sweep is not a failure, got %v", err)
	}
	if guard.calls != 1 || len(svc.sweepCalls) != 0 {
		t.Fatalf("expected no sweeps while halted, got %+v", svc.sweepCalls)
	}
}

func TestHandleSweep_GuardErrorFailsClosed(t *testing.T) {
	svc := sweepableService()
	w := treasury.NewWorker(svc).WithSweepGuard(&fakeGuard{err: errors.New("db down")})

	if err := w.HandleSweep(context.Background(), &asynq.Task{}); err == nil {
		t.Fatal("expected the halt-check error to be returned")
	}
	if len(svc.sweepCalls) != 0 {
		t.Fatalf("expected no sweeps when the halt state is unknown, got %+v", svc.sweepCalls)
	}
}

func TestHandleSweep_NotHaltedSweeps(t *testing.T) {
	svc := sweepableService()
	w := treasury.NewWorker(svc).WithSweepGuard(&fakeGuard{})

	if err := w.HandleSweep(context.Background(), &asynq.Task{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(svc.sweepCalls) != 1 {
		t.Fatalf("expected one sweep, got %+v", svc.sweepCalls)
	}
}

func TestHandleCustodialSweep_HaltedByInvariantSkipsSweeper(t *testing.T) {
	sweeper := &fakeSweeper{}
	w := treasury.NewWorker(&fakeService{}).WithCustodialSweeper(sweeper).WithSweepGuard(&fakeGuard{halted: true})

	if err := w.HandleCustodialSweep(context.Background(), asynq.NewTask("wallet:custodial_sweep", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sweeper.calls != 0 {
		t.Fatalf("custodial sweeper ran %d times while halted", sweeper.calls)
	}
}
