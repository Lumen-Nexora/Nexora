package fees_test

import (
	"context"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fees"
	"github.com/Lumen-Nexora/Nexora/internal/postgres"
	"github.com/shopspring/decimal"
)

type mockRepo struct {
	fees.Repository
	volume decimal.Decimal
	tier   *domain.FeeTier
}

func (m *mockRepo) GetSchedule(ctx context.Context, tenantID *string, asset string) (*domain.FeeSchedule, error) {
	return &domain.FeeSchedule{
		TransferFeeBps:   100,
		ConversionFeeBps: 150,
		MinFeeAmount:     decimal.Zero,
	}, nil
}

func (m *mockRepo) GetMonthlyVolume(ctx context.Context, tenantID string) (decimal.Decimal, error) {
	return m.volume, nil
}

func (m *mockRepo) GetApplicableTier(ctx context.Context, tenantID string, volume decimal.Decimal) *domain.FeeTier {
	return m.tier
}

func TestCalculateFee_TierBoundaries(t *testing.T) {
	tier := &domain.FeeTier{
		TransferFeeBps:   50,
		ConversionFeeBps: 80,
	}

	tests := []struct {
		name        string
		volume      decimal.Decimal
		tier        *domain.FeeTier
		isTransfer  bool
		expectedBps int
	}{
		{
			name:        "no tier fallback to schedule transfer",
			volume:      decimal.NewFromInt(0),
			tier:        nil,
			isTransfer:  true,
			expectedBps: 100,
		},
		{
			name:        "no tier fallback to schedule conversion",
			volume:      decimal.NewFromInt(0),
			tier:        nil,
			isTransfer:  false,
			expectedBps: 150,
		},
		{
			name:        "tier applies transfer",
			volume:      decimal.NewFromInt(100000),
			tier:        tier,
			isTransfer:  true,
			expectedBps: 50,
		},
		{
			name:        "tier applies conversion",
			volume:      decimal.NewFromInt(100000),
			tier:        tier,
			isTransfer:  false,
			expectedBps: 80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &mockRepo{
				volume: tt.volume,
				tier:   tt.tier,
			}
			svc := fees.NewService(repo)

			var fee *fees.TransferFee
			var err error
			amount := decimal.NewFromInt(1000)

			if tt.isTransfer {
				fee, err = svc.CalculateTransferFee(context.Background(), "tenant1", "USDC", amount)
			} else {
				fee, err = svc.CalculateConversionFee(context.Background(), "tenant1", "USDC", amount)
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fee.FeeBps != tt.expectedBps {
				t.Errorf("expected %d bps, got %d", tt.expectedBps, fee.FeeBps)
			}
		})
	}
}

func TestFeeRepo_ImplementsRepository(t *testing.T) {
	var _ fees.Repository = (*postgres.FeeRepo)(nil)
}
