package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

type travelRuleGateDouble struct {
	required      bool
	requiresErr   error
	beginStatus   domain.TravelRuleExchangeStatus
	beginErr      error
	releaseStatus domain.TransactionStatus
	beginCalls    int
}

func (g *travelRuleGateDouble) Requires(context.Context, string, decimal.Decimal, *domain.TravelRuleRequest) (bool, error) {
	return g.required, g.requiresErr
}

func (g *travelRuleGateDouble) Begin(_ context.Context, tx *domain.Transaction, _ *domain.TravelRuleRequest, releaseStatus domain.TransactionStatus) (domain.TravelRuleExchangeStatus, error) {
	g.beginCalls++
	g.releaseStatus = releaseStatus
	if tx.Status != domain.StatusComplianceHold {
		return "", errors.New("Travel Rule transfer was not persisted as a hold")
	}
	return g.beginStatus, g.beginErr
}

func travelRuleTestRequest() *domain.TravelRuleRequest {
	return &domain.TravelRuleRequest{
		RecipientVASPID: "trusted-vasp",
		IVMS101: json.RawMessage(`{"originator":{},"beneficiary":{}}`),
	}
}

func runTravelRuleTransfer(t *testing.T, gate *travelRuleGateDouble, screener Screener) (*domain.Transaction, *screeningMockTxRepo, error) {
	t.Helper()
	repo := &screeningMockTxRepo{}
	svc := newScreeningService(repo, screener)
	svc = ConfigureTravelRuleGate(svc, gate)
	extended := svc.(interface {
		InitiateTransferExt(context.Context, TransferParams) (*domain.Transaction, error)
	})
	tx, err := extended.InitiateTransferExt(context.Background(), TransferParams{
		FromID: "wallet-a", ToID: "wallet-b", Asset: "USDC",
		Amount: decimal.NewFromInt(1200), TravelRule: travelRuleTestRequest(),
	})
	return tx, repo, err
}

func TestTravelRuleGatePersistsTransferOnHoldUntilAsyncConfirmation(t *testing.T) {
	gate := &travelRuleGateDouble{required: true, beginStatus: domain.TravelRuleExchangePending}
	tx, repo, err := runTravelRuleTransfer(t, gate, nil)
	if err != nil {
		t.Fatalf("InitiateTransferExt: %v", err)
	}
	if tx.Status != domain.StatusComplianceHold {
		t.Fatalf("status = %q, want compliance_hold", tx.Status)
	}
	if repo.createdCount() != 1 || gate.beginCalls != 1 {
		t.Fatalf("created=%d Begin calls=%d, want 1 each", repo.createdCount(), gate.beginCalls)
	}
	if gate.releaseStatus != domain.StatusPending {
		t.Fatalf("release status = %q, want pending", gate.releaseStatus)
	}
}

func TestTravelRuleGateReleasesOnlyAfterImmediateVerifiedConfirmation(t *testing.T) {
	gate := &travelRuleGateDouble{required: true, beginStatus: domain.TravelRuleExchangeConfirmed}
	tx, _, err := runTravelRuleTransfer(t, gate, nil)
	if err != nil {
		t.Fatalf("InitiateTransferExt: %v", err)
	}
	if tx.Status != domain.StatusPending {
		t.Fatalf("status = %q, want pending after confirmation", tx.Status)
	}
}

func TestTravelRuleGateDoesNotClearIndependentScreeningHold(t *testing.T) {
	gate := &travelRuleGateDouble{required: true, beginStatus: domain.TravelRuleExchangeConfirmed}
	screener := &screeningMockScreener{decision: decisionOf(domain.ScreeningHold, "sanctions_review")}
	tx, _, err := runTravelRuleTransfer(t, gate, screener)
	if err != nil {
		t.Fatalf("InitiateTransferExt: %v", err)
	}
	if tx.Status != domain.StatusComplianceHold {
		t.Fatalf("status = %q, want independent compliance hold to remain", tx.Status)
	}
	if gate.releaseStatus != domain.StatusComplianceHold {
		t.Fatalf("release status = %q, want compliance_hold", gate.releaseStatus)
	}
}

func TestTravelRuleGateFailsClosedWhenValuationOrDetailsFail(t *testing.T) {
	gate := &travelRuleGateDouble{required: true, requiresErr: errors.New("missing IVMS 101")}
	tx, repo, err := runTravelRuleTransfer(t, gate, nil)
	if err == nil || tx != nil {
		t.Fatalf("transfer = (%v, %v), want pre-persistence error", tx, err)
	}
	if repo.createdCount() != 0 {
		t.Fatalf("created %d transactions despite failed Travel Rule admission", repo.createdCount())
	}
}

func TestTravelRuleRejectionFailsTransferWithoutQueueing(t *testing.T) {
	gate := &travelRuleGateDouble{required: true, beginStatus: domain.TravelRuleExchangeRejected}
	tx, _, err := runTravelRuleTransfer(t, gate, nil)
	if err != nil {
		t.Fatalf("InitiateTransferExt: %v", err)
	}
	if tx.Status != domain.StatusFailed {
		t.Fatalf("status = %q, want failed", tx.Status)
	}
}
