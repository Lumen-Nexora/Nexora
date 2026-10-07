package treasury_test

import (
	"context"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/treasury"
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
