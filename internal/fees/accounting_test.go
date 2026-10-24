package fees

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

type fakeNetworkFeeRepo struct {
	Repository
	charges      map[string]*domain.NetworkFeeCharge
	recordCalls  int
	monthly      *domain.NetworkFeeCOGSReport
	monthlyStart time.Time
	monthlyEnd   time.Time
}

func (r *fakeNetworkFeeRepo) RecordNetworkFee(_ context.Context, charge *domain.NetworkFeeCharge) error {
	r.recordCalls++
	if _, exists := r.charges[charge.TxHash]; exists {
		return nil
	}
	r.charges[charge.TxHash] = charge
	return nil
}

func (r *fakeNetworkFeeRepo) GetNetworkFeeByTxHash(_ context.Context, txHash string) (*domain.NetworkFeeCharge, error) {
	charge, ok := r.charges[txHash]
	if !ok {
		return nil, domain.ErrNetworkFeeNotFound
	}
	return charge, nil
}

func (r *fakeNetworkFeeRepo) GetMonthlyNetworkFeeCOGS(_ context.Context, start, end time.Time) (*domain.NetworkFeeCOGSReport, error) {
	r.monthlyStart = start
	r.monthlyEnd = end
	if r.monthly != nil {
		return r.monthly, nil
	}
	return &domain.NetworkFeeCOGSReport{}, nil
}

type fakeHistoricalRate struct {
	rate decimal.Decimal
	err  error
	at   time.Time
}

func (r *fakeHistoricalRate) RateAt(_ context.Context, _, _ string, at time.Time) (decimal.Decimal, error) {
	r.at = at
	return r.rate, r.err
}

func TestCaptureNetworkFeeValuesAtLedgerClose(t *testing.T) {
	closedAt := time.Date(2026, 10, 22, 12, 34, 56, 0, time.UTC)
	tenantID := "tenant-1"
	repo := &fakeNetworkFeeRepo{charges: make(map[string]*domain.NetworkFeeCharge)}
	rates := &fakeHistoricalRate{rate: decimal.RequireFromString("0.25")}
	accounting := NewNetworkFeeAccounting(repo, rates)

	charge, err := accounting.CaptureNetworkFee(context.Background(), NetworkFeeEvent{
		TransactionID:     "tx-1",
		TxHash:            "hash-1",
		TenantID:          &tenantID,
		LedgerSequence:    123,
		LedgerClosedAt:    closedAt,
		FeeChargedStroops: 10_000_000,
	})
	if err != nil {
		t.Fatalf("CaptureNetworkFee: %v", err)
	}

	if !rates.at.Equal(closedAt) {
		t.Fatalf("rate lookup time = %s, want ledger close %s", rates.at, closedAt)
	}
	if !charge.XLMAmount.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("XLM amount = %s, want 1", charge.XLMAmount)
	}
	if charge.USDAmount == nil || !charge.USDAmount.Equal(decimal.RequireFromString("0.25")) {
		t.Fatalf("USD amount = %v, want 0.25", charge.USDAmount)
	}
	if !charge.Valued {
		t.Fatal("charge must be valued when a historical rate is available")
	}
	if charge.Allocation != domain.NetworkFeeAllocationTenant {
		t.Fatalf("allocation = %s, want tenant", charge.Allocation)
	}
	if charge.CostCenter != networkFeeCostCenter {
		t.Fatalf("cost center = %s, want %s", charge.CostCenter, networkFeeCostCenter)
	}
	if len(repo.charges) != 1 {
		t.Fatalf("recorded charges = %d, want 1", len(repo.charges))
	}
}

func TestCaptureNetworkFeeLeavesMissingRateUnvalued(t *testing.T) {
	repo := &fakeNetworkFeeRepo{charges: make(map[string]*domain.NetworkFeeCharge)}
	rates := &fakeHistoricalRate{err: errors.New("no historical quote")}
	accounting := NewNetworkFeeAccounting(repo, rates)

	charge, err := accounting.CaptureNetworkFee(context.Background(), NetworkFeeEvent{
		TxHash:            "hash-unvalued",
		LedgerSequence:    124,
		LedgerClosedAt:    time.Now().UTC(),
		FeeChargedStroops: 100,
	})
	if err != nil {
		t.Fatalf("CaptureNetworkFee: %v", err)
	}
	if charge.Valued {
		t.Fatal("charge with a missing historical rate must be unvalued")
	}
	if charge.XLMUSDRate != nil || charge.USDAmount != nil {
		t.Fatal("unvalued charge must not carry a rate or USD amount")
	}
	if charge.Allocation != domain.NetworkFeeAllocationOverhead {
		t.Fatalf("allocation = %s, want platform overhead", charge.Allocation)
	}
}

func TestCaptureNetworkFeeIsIdempotentOnTxHash(t *testing.T) {
	repo := &fakeNetworkFeeRepo{charges: make(map[string]*domain.NetworkFeeCharge)}
	accounting := NewNetworkFeeAccounting(repo, &fakeHistoricalRate{rate: decimal.NewFromInt(1)})
	event := NetworkFeeEvent{
		TxHash:            "hash-replayed",
		LedgerSequence:    125,
		LedgerClosedAt:    time.Now().UTC(),
		FeeChargedStroops: 100,
	}

	first, err := accounting.CaptureNetworkFee(context.Background(), event)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	second, err := accounting.CaptureNetworkFee(context.Background(), event)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}

	if repo.recordCalls != 1 {
		t.Fatalf("record calls = %d, want 1", repo.recordCalls)
	}
	if first.ID != second.ID {
		t.Fatalf("replay returned a new charge: %s != %s", first.ID, second.ID)
	}
}

func TestGetMonthlyNetworkFeeCOGSUsesCalendarMonthAndAllocations(t *testing.T) {
	repo := &fakeNetworkFeeRepo{charges: make(map[string]*domain.NetworkFeeCharge)}
	repo.monthly = &domain.NetworkFeeCOGSReport{
		TransactionCount: 2,
		Lines: []domain.NetworkFeeCOGSLine{
			{Allocation: domain.NetworkFeeAllocationTenant, TransactionCount: 1, XLMAmount: decimal.RequireFromString("0.5"), USDAmount: decimal.RequireFromString("0.1")},
			{Allocation: domain.NetworkFeeAllocationOverhead, TransactionCount: 1, XLMAmount: decimal.RequireFromString("0.25"), USDAmount: decimal.RequireFromString("0.05")},
		},
	}
	accounting := NewNetworkFeeAccounting(repo, nil)

	report, err := accounting.GetMonthlyNetworkFeeCOGS(context.Background(), time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GetMonthlyNetworkFeeCOGS: %v", err)
	}

	wantStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !repo.monthlyStart.Equal(wantStart) {
		t.Fatalf("start = %s, want %s", repo.monthlyStart, wantStart)
	}
	if !repo.monthlyEnd.Equal(wantStart.AddDate(0, 1, 0)) {
		t.Fatalf("end = %s, want %s", repo.monthlyEnd, wantStart.AddDate(0, 1, 0))
	}
	if report.Month != "2026-10" {
		t.Fatalf("month = %s, want 2026-10", report.Month)
	}
	if len(report.Allocations) != 2 {
		t.Fatalf("allocation totals = %d, want 2", len(report.Allocations))
	}
	if !report.Allocations[0].USDAmount.Equal(decimal.RequireFromString("0.1")) &&
		!report.Allocations[1].USDAmount.Equal(decimal.RequireFromString("0.1")) {
		t.Fatal("allocation totals must preserve line USD amounts")
	}
}
