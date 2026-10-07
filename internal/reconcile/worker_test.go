package reconcile

import (
	"context"
	"encoding/json"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"testing"
	"time"
)

type fakeForceSettleRepo struct {
	Repository
	settledID string
}

func (f *fakeForceSettleRepo) GetConfirmedTxesForReconciliation(_ context.Context, _ time.Duration) ([]*domain.Transaction, error) {
	return nil, nil
}

func TestWorkerPayloadRoundTrip(t *testing.T) {
	fsPayload := ForceSettlePayload{
		TransferID: "tx-123",
		Actor:      "admin-1",
	}
	raw, err := json.Marshal(fsPayload)
	if err != nil {
		t.Fatalf("marshal force settle payload: %v", err)
	}
	var decodedFS ForceSettlePayload
	if err := json.Unmarshal(raw, &decodedFS); err != nil {
		t.Fatalf("unmarshal force settle payload: %v", err)
	}
	if decodedFS.TransferID != fsPayload.TransferID || decodedFS.Actor != fsPayload.Actor {
		t.Fatalf("force settle payload roundtrip failed: %+v", decodedFS)
	}

	wPayload := WalletReconcilePayload{
		WalletID: "wallet-123",
		Actor:    "admin-1",
	}
	rawW, err := json.Marshal(wPayload)
	if err != nil {
		t.Fatalf("marshal wallet reconcile payload: %v", err)
	}
	var decodedW WalletReconcilePayload
	if err := json.Unmarshal(rawW, &decodedW); err != nil {
		t.Fatalf("unmarshal wallet reconcile payload: %v", err)
	}
	if decodedW.WalletID != wPayload.WalletID || decodedW.Actor != wPayload.Actor {
		t.Fatalf("wallet reconcile payload roundtrip failed: %+v", decodedW)
	}
}
