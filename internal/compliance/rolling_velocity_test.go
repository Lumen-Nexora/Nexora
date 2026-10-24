package compliance

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

func newTestVelocityLimiter(t *testing.T) (*RedisVelocityLimiter, *miniredis.Miniredis, func()) {
	t.Helper()
	server, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	return NewRedisVelocityLimiter(client), server, func() {
		_ = client.Close()
		server.Close()
	}
}

func velocityRequest(id string) domain.ScreeningRequest {
	return domain.ScreeningRequest{
		OrgID:        "tenant-1",
		TransferID:   id,
		AccountType:  domain.AccountTypeIndividual,
		RiskCategory: domain.RiskCategoryMedium,
		Asset:        "USDC",
		Amount:       decimal.NewFromInt(1),
	}
}

func TestRedisVelocityLimiterAtomicallyAdmitsAtMostLimitWith1000ConcurrentRequests(t *testing.T) {
	limiter, _, cleanup := newTestVelocityLimiter(t)
	defer cleanup()

	const requests = 1000
	var admitted atomic.Int64
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := limiter.Reserve(context.Background(), velocityRequest(fmt.Sprintf("tx-%d", i)))
			switch err {
			case nil:
				admitted.Add(1)
			case ErrVelocityLimitExceeded:
			default:
				unexpected.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := admitted.Load(); got != 5 {
		t.Fatalf("admitted = %d, want exactly the 1-hour count ceiling of 5", got)
	}
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("unexpected reservation errors = %d", got)
	}
}

func TestRedisVelocityLimiterUsesAccountAndRiskMultipliers(t *testing.T) {
	tests := []struct {
		name        string
		accountType domain.AccountType
		risk        domain.RiskCategory
		countLimit  int
	}{
		{"individual low risk", domain.AccountTypeIndividual, domain.RiskCategoryLow, 7},
		{"individual high risk", domain.AccountTypeIndividual, domain.RiskCategoryHigh, 2},
		{"organization medium risk", domain.AccountTypeOrganization, domain.RiskCategoryMedium, 10},
		{"organization low risk", domain.AccountTypeOrganization, domain.RiskCategoryLow, 15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limiter, _, cleanup := newTestVelocityLimiter(t)
			defer cleanup()
			for i := 0; i < tc.countLimit; i++ {
				req := velocityRequest(fmt.Sprintf("tx-%d", i))
				req.AccountType = tc.accountType
				req.RiskCategory = tc.risk
				if err := limiter.Reserve(context.Background(), req); err != nil {
					t.Fatalf("reservation %d failed before the tier ceiling: %v", i+1, err)
				}
			}
			req := velocityRequest("over-limit")
			req.AccountType = tc.accountType
			req.RiskCategory = tc.risk
			if err := limiter.Reserve(context.Background(), req); err != ErrVelocityLimitExceeded {
				t.Fatalf("over-limit reservation error = %v, want ErrVelocityLimitExceeded", err)
			}
		})
	}
}

func TestRedisVelocityLimiterEnforcesVolumeAndSupportsFinalizeRollback(t *testing.T) {
	limiter, _, cleanup := newTestVelocityLimiter(t)
	defer cleanup()

	for i := 0; i < 7; i++ {
		req := velocityRequest(fmt.Sprintf("tx-%d", i))
		req.RiskCategory = domain.RiskCategoryLow
		req.Amount = decimal.NewFromInt(200)
		if err := limiter.Reserve(context.Background(), req); err != nil {
			t.Fatalf("reservation %d failed: %v", i+1, err)
		}
		if i == 0 {
			if err := limiter.Finalize(context.Background(), req.OrgID, req.Asset, req.TransferID); err != nil {
				t.Fatalf("finalize reservation: %v", err)
			}
		}
	}

	overVolume := velocityRequest("over-volume")
	overVolume.RiskCategory = domain.RiskCategoryLow
	overVolume.Amount = decimal.NewFromInt(200)
	if err := limiter.Reserve(context.Background(), overVolume); err != ErrVelocityLimitExceeded {
		t.Fatalf("volume overflow error = %v, want ErrVelocityLimitExceeded", err)
	}

	if err := limiter.Rollback(context.Background(), "tenant-1", "USDC", "tx-0"); err != nil {
		t.Fatalf("rollback reservation: %v", err)
	}
	if err := limiter.Rollback(context.Background(), "tenant-1", "USDC", "tx-0"); err != nil {
		t.Fatalf("repeated rollback should be idempotent: %v", err)
	}
	if err := limiter.Reserve(context.Background(), overVolume); err != nil {
		t.Fatalf("reservation should be admitted after rollback: %v", err)
	}
}

func TestRedisVelocityLimiterUsesMicrosecondWindowBoundary(t *testing.T) {
	limiter, server, cleanup := newTestVelocityLimiter(t)
	defer cleanup()

	now := time.Date(2026, time.October, 22, 10, 0, 0, 0, time.UTC)
	server.SetTime(now)
	for i := 0; i < 5; i++ {
		if err := limiter.Reserve(context.Background(), velocityRequest(fmt.Sprintf("tx-%d", i))); err != nil {
			t.Fatalf("reservation %d failed: %v", i+1, err)
		}
	}

	now = now.Add(time.Hour - time.Microsecond)
	server.SetTime(now)
	if err := limiter.Reserve(context.Background(), velocityRequest("before-boundary")); err != ErrVelocityLimitExceeded {
		t.Fatalf("reservation before boundary error = %v, want ErrVelocityLimitExceeded", err)
	}
	now = now.Add(time.Microsecond)
	server.SetTime(now)
	if err := limiter.Reserve(context.Background(), velocityRequest("at-boundary")); err != nil {
		t.Fatalf("reservation at exact 1-hour boundary failed: %v", err)
	}
}

func TestEffectiveVelocityLimitsRejectInvalidTierAndAmount(t *testing.T) {
	if _, _, err := effectiveVelocityLimits("unknown", domain.RiskCategoryMedium); err == nil {
		t.Fatal("unknown account type should fail closed")
	}
	if _, _, err := effectiveVelocityLimits(domain.AccountTypeIndividual, "unknown"); err == nil {
		t.Fatal("unknown risk category should fail closed")
	}
	if _, err := amountToVelocityUnits(decimal.RequireFromString("0.000000001")); err == nil {
		t.Fatal("amounts with more than 8 decimal places should be rejected")
	}
}
