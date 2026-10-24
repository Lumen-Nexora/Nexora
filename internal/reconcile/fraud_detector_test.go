package reconcile

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSecurityAlerter struct {
	mu     sync.Mutex
	alerts []alerting.SecurityAlert
}

func (r *recordingSecurityAlerter) Send(_ context.Context, alert alerting.SecurityAlert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, alert)
	return nil
}

func (r *recordingSecurityAlerter) Alerts() []alerting.SecurityAlert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]alerting.SecurityAlert, len(r.alerts))
	copy(out, r.alerts)
	return out
}

func TestFraudDetectorFlagsDiscrepancyFrequencyAnomaly(t *testing.T) {
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewMemorySecurityActionStore()
	alerter := &recordingSecurityAlerter{}
	detector := NewFraudDetector(FraudConfig{
		Bucket:             time.Minute,
		BaselineBuckets:    5,
		FrequencyZScore:    3,
		MinFrequencyEvents: 3,
		Window:             time.Hour,
	}, store, alerter).WithClock(func() time.Time { return now })

	ctx := context.Background()
	// Six quiet minutes establish a baseline of one discrepancy per minute.
	for i := 0; i < 6; i++ {
		now = base.Add(time.Duration(i) * time.Minute)
		_, err := detector.Observe(ctx, FraudObservation{
			TenantID: "tenant-1", AccountID: "account-1", WalletID: "wallet-1",
			RouteID: "wallet-1", Asset: "USDC",
			TransferAmount: decimal.NewFromInt(1000),
			Discrepancy:    decimal.NewFromInt(5),
			ObservedAt:     now,
		})
		require.NoError(t, err)
	}

	// A burst of eight discrepancies in one minute is a statistical anomaly.
	now = base.Add(6 * time.Minute)
	var findings []FraudFinding
	for i := 0; i < 8; i++ {
		got, err := detector.Observe(ctx, FraudObservation{
			TenantID: "tenant-1", AccountID: "account-1", WalletID: "wallet-1",
			RouteID: "wallet-1", Asset: "USDC",
			TransferAmount: decimal.NewFromInt(1000),
			Discrepancy:    decimal.NewFromInt(5),
			ObservedAt:     now,
		})
		require.NoError(t, err)
		findings = append(findings, got...)
	}

	require.NotEmpty(t, findings)
	assert.Equal(t, FindingFrequencyAnomaly, findings[0].Kind)
	assert.GreaterOrEqual(t, findings[0].Score, 3.0)
	assert.Equal(t, "account-1", findings[0].AccountID)

	flags := store.Flags()
	require.Len(t, flags, 1)
	assert.Equal(t, SecurityActionReview, flags[0].Action)

	alerts := alerter.Alerts()
	require.Len(t, alerts, 1)
	assert.Contains(t, alerts[0].Summary, string(FindingFrequencyAnomaly))
}

func TestFraudDetectorFlagsMicroVarianceSkimmingAndFreezesSweeps(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	store := NewMemorySecurityActionStore()
	alerter := &recordingSecurityAlerter{}
	detector := NewFraudDetector(FraudConfig{
		Window:               10 * time.Minute,
		MicroVarianceMax:     decimal.RequireFromString("0.01"),
		MicroVarianceRatio:   decimal.RequireFromString("0.001"),
		MinSkimmingEvents:    10,
		HighVolumeTransfers:  20,
		HighValueSweepAmount: decimal.RequireFromString("1000"),
	}, store, alerter).WithClock(func() time.Time { return now })

	ctx := context.Background()
	var findings []FraudFinding
	// Twenty high-value transfers each losing 0.002 is classic skimming: no
	// single discrepancy is large enough to trip the normal drift threshold.
	for i := 0; i < 20; i++ {
		got, err := detector.Observe(ctx, FraudObservation{
			TenantID: "tenant-2", AccountID: "account-2", WalletID: "wallet-2",
			RouteID: "wallet-2->corridor-usdc", Asset: "USDC",
			TransferAmount: decimal.NewFromInt(100000),
			Discrepancy:    decimal.RequireFromString("0.002"),
			ObservedAt:     now,
		})
		require.NoError(t, err)
		findings = append(findings, got...)
	}

	require.NotEmpty(t, findings)
	assert.Equal(t, FindingMicroVariance, findings[0].Kind)
	assert.Equal(t, 20, findings[0].Count)
	assert.True(t, findings[0].TotalSkimmed.Equal(decimal.RequireFromString("0.04")))

	flags := store.Flags()
	require.Len(t, flags, 2)
	assert.Equal(t, SecurityActionReview, flags[0].Action)
	assert.Equal(t, SecurityActionFreeze, flags[1].Action)

	alerts := alerter.Alerts()
	require.Len(t, alerts, 1)
	assert.Equal(t, alerting.SeverityCritical, alerts[0].Severity)
	assert.Equal(t, "wallet-2->corridor-usdc", alerts[0].Details["route_id"])
}

func TestFraudDetectorIgnoresOrdinaryDiscrepancies(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	store := NewMemorySecurityActionStore()
	detector := NewFraudDetector(FraudConfig{
		Window:              10 * time.Minute,
		MicroVarianceMax:    decimal.RequireFromString("0.01"),
		MicroVarianceRatio:  decimal.RequireFromString("0.001"),
		MinSkimmingEvents:   10,
		HighVolumeTransfers: 20,
	}, store, nil).WithClock(func() time.Time { return now })

	ctx := context.Background()
	for i := 0; i < 25; i++ {
		findings, err := detector.Observe(ctx, FraudObservation{
			TenantID: "tenant-3", AccountID: "account-3", WalletID: "wallet-3",
			RouteID: "wallet-3", Asset: "USDC",
			TransferAmount: decimal.NewFromInt(1000),
			Discrepancy:    decimal.NewFromInt(50),
			ObservedAt:     now,
		})
		require.NoError(t, err)
		assert.Empty(t, findings)
	}

	assert.Empty(t, store.Flags())
}

func TestFraudDetectorIgnoresMicroVarianceBelowVolumeThreshold(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	store := NewMemorySecurityActionStore()
	detector := NewFraudDetector(FraudConfig{
		Window:              10 * time.Minute,
		MicroVarianceMax:    decimal.RequireFromString("0.01"),
		MicroVarianceRatio:  decimal.RequireFromString("0.001"),
		MinSkimmingEvents:   5,
		HighVolumeTransfers: 100,
	}, store, nil).WithClock(func() time.Time { return now })

	ctx := context.Background()
	for i := 0; i < 15; i++ {
		findings, err := detector.Observe(ctx, FraudObservation{
			TenantID: "tenant-4", AccountID: "account-4", WalletID: "wallet-4",
			RouteID: "wallet-4", Asset: "USDC",
			TransferAmount: decimal.NewFromInt(1000),
			Discrepancy:    decimal.RequireFromString("0.002"),
			ObservedAt:     now,
		})
		require.NoError(t, err)
		assert.Empty(t, findings)
	}

	assert.Empty(t, store.Flags())
}
