package settlement

import (
	"context"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

type velocityLifecycleStub struct {
	finalized  string
	rolledBack string
}

func (s *velocityLifecycleStub) Reserve(context.Context, domain.ScreeningRequest) error {
	return nil
}

func (s *velocityLifecycleStub) Finalize(_ context.Context, _, _, transferID string) error {
	s.finalized = transferID
	return nil
}

func (s *velocityLifecycleStub) Rollback(_ context.Context, _, _, transferID string) error {
	s.rolledBack = transferID
	return nil
}

func TestSettlementFinalizesAndRollsBackVelocityReservation(t *testing.T) {
	limiter := &velocityLifecycleStub{}
	engine := &Engine{velocity: limiter}
	tenantID := "tenant-1"
	tx := &domain.Transaction{ID: "transfer-1", TenantID: &tenantID, Asset: "USDC"}

	engine.finalizeVelocity(context.Background(), tx)
	if limiter.finalized != tx.ID {
		t.Fatalf("finalized transfer = %q, want %q", limiter.finalized, tx.ID)
	}

	engine.rollbackVelocity(context.Background(), tx)
	if limiter.rolledBack != tx.ID {
		t.Fatalf("rolled back transfer = %q, want %q", limiter.rolledBack, tx.ID)
	}
}
