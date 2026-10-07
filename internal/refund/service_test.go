package refund

import (
	"context"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/Lumen-Nexora/Nexora/internal/transfer"
	"github.com/shopspring/decimal"
)

type refundRepoFake struct{ record *Record }

func (f *refundRepoFake) Reserve(_ context.Context, original string, amount decimal.Decimal, reason, key string) (*Record, error) {
	f.record = &Record{ID: "refund-1", OriginalTransactionID: original, FromWallet: "recipient", ToWallet: "sender", Asset: "USDC", Amount: amount, Reason: reason, Status: "requested", IdempotencyKey: key}
	return f.record, nil
}
func (f *refundRepoFake) LinkTransaction(_ context.Context, _, txID, status string) error {
	f.record.RefundTransactionID, f.record.Status = txID, status
	return nil
}
func (f *refundRepoFake) MarkFailed(context.Context, string) error {
	f.record.Status = "failed"
	return nil
}
func (f *refundRepoFake) Get(context.Context, string) (*Record, error) { return f.record, nil }
func (f *refundRepoFake) ListByOriginal(context.Context, string) ([]*Record, error) {
	return []*Record{f.record}, nil
}

type transferFake struct{ params transfer.TransferParams }

func (f *transferFake) InitiateTransferExt(_ context.Context, params transfer.TransferParams) (*domain.Transaction, error) {
	f.params = params
	return &domain.Transaction{ID: "tx-refund", Status: domain.StatusPending}, nil
}
func (f *transferFake) GetTransaction(context.Context, string) (*domain.Transaction, error) {
	return &domain.Transaction{Status: domain.StatusSubmitted}, nil
}

func TestCreateIssuesReverseTransferAndTracksPending(t *testing.T) {
	repo := &refundRepoFake{}
	transfers := &transferFake{}
	svc := NewService(repo, transfers)
	ctx := tenant.WithMode(tenant.WithID(context.Background(), "tenant-1"), domain.ModeTest)
	amount := decimal.RequireFromString("12.5")
	record, err := svc.Create(ctx, "original-1", amount, "customer return", "refund-key")
	if err != nil {
		t.Fatalf("create refund: %v", err)
	}
	if record.Status != "pending" || record.RefundTransactionID != "tx-refund" {
		t.Fatalf("unexpected refund lifecycle record: %+v", record)
	}
	if transfers.params.FromID != "recipient" || transfers.params.ToID != "sender" || !transfers.params.Amount.Equal(amount) {
		t.Fatalf("refund did not reverse transaction direction: %+v", transfers.params)
	}
	if transfers.params.IdempotencyKey != "refund:refund-1" {
		t.Fatalf("unexpected reverse-transfer idempotency key: %s", transfers.params.IdempotencyKey)
	}
}

func TestRefundStatusMapsSettlementStates(t *testing.T) {
	cases := map[domain.TransactionStatus]string{
		domain.StatusPending: "pending", domain.StatusSubmitted: "pending",
		domain.StatusConfirmed: "succeeded", domain.StatusSettled: "succeeded",
		domain.StatusFailed: "failed", domain.StatusCancelled: "failed",
	}
	for status, expected := range cases {
		if got := refundStatus(status); got != expected {
			t.Errorf("refundStatus(%q) = %q, want %q", status, got, expected)
		}
	}
}
