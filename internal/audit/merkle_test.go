package audit

import (
	"context"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

func TestFilterSettledTransactionsUsesUTCWindow(t *testing.T) {
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	txs := []*domain.Transaction{
		{ID: "before", Status: domain.StatusSettled, CreatedAt: time.Date(2026, 10, 21, 23, 59, 59, 0, time.UTC)},
		{ID: "inside-1", Status: domain.StatusSettled, CreatedAt: time.Date(2026, 10, 22, 00, 00, 00, 0, time.UTC)},
		{ID: "inside-2", Status: domain.StatusSettled, CreatedAt: time.Date(2026, 10, 22, 12, 30, 0, 0, time.UTC)},
		{ID: "after", Status: domain.StatusSettled, CreatedAt: time.Date(2026, 10, 23, 00, 00, 00, 0, time.UTC)},
		{ID: "pending", Status: domain.StatusPending, CreatedAt: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)},
	}

	got := FilterSettledTransactions(txs, day)
	if len(got) != 2 {
		t.Fatalf("len(FilterSettledTransactions) = %d, want 2", len(got))
	}
	if got[0].ID != "inside-1" || got[1].ID != "inside-2" {
		t.Fatalf("unexpected order: %#v", []string{got[0].ID, got[1].ID})
	}
}

func TestProofForSettledTransactionVerifiesAndRejectsTampering(t *testing.T) {
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	txs := []*domain.Transaction{
		{ID: "tx-a", TxHash: "hash-a", Status: domain.StatusSettled, Type: domain.TypeTransfer, Asset: "USDC", FromWallet: "w1", ToWallet: "w2", CreatedAt: time.Date(2026, 10, 22, 0, 10, 0, 0, time.UTC)},
		{ID: "tx-b", TxHash: "hash-b", Status: domain.StatusSettled, Type: domain.TypeTransfer, Asset: "USDC", FromWallet: "w2", ToWallet: "w3", CreatedAt: time.Date(2026, 10, 22, 12, 15, 0, 0, time.UTC)},
		{ID: "tx-c", TxHash: "hash-c", Status: domain.StatusSettled, Type: domain.TypeTransfer, Asset: "USD", FromWallet: "w3", ToWallet: "w4", CreatedAt: time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC)},
	}

	proof, err := ProofForSettledTransaction(day, txs, "tx-b")
	if err != nil {
		t.Fatalf("ProofForSettledTransaction: %v", err)
	}
	if proof.TransactionID != "tx-b" {
		t.Fatalf("transaction id mismatch: got %q, want %q", proof.TransactionID, "tx-b")
	}

	root, err := MerkleRoot([][32]byte{SettlementLeaf(txs[0]), SettlementLeaf(txs[1]), SettlementLeaf(txs[2])})
	if err != nil {
		t.Fatalf("MerkleRoot: %v", err)
	}
	path, err := ParseProof(proof.Proof)
	if err != nil {
		t.Fatalf("ParseProof: %v", err)
	}
	if !VerifyProof(SettlementLeaf(txs[1]), path, root) {
		t.Fatal("valid proof should verify")
	}

	badLeaf := SettlementLeaf(&domain.Transaction{ID: "tx-b", TxHash: "tampered", Status: domain.StatusSettled, Type: domain.TypeTransfer, Asset: "USDC", FromWallet: "w2", ToWallet: "w3", CreatedAt: txs[1].CreatedAt})
	if VerifyProof(badLeaf, path, root) {
		t.Fatal("tampered leaf must be rejected")
	}
}

func TestGetProofReadsFromSettlementSource(t *testing.T) {
	txs := []*domain.Transaction{
		{ID: "tx-1", TxHash: "hash-1", Status: domain.StatusSettled, Type: domain.TypeTransfer, Asset: "USDC", CreatedAt: time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)},
		{ID: "tx-2", TxHash: "hash-2", Status: domain.StatusSettled, Type: domain.TypeTransfer, Asset: "USDC", CreatedAt: time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)},
	}

	source := &stubSettlementSource{txs: txs}
	proof, err := GetProof(context.Background(), source, "tx-2")
	if err != nil {
		t.Fatalf("GetProof: %v", err)
	}
	if proof.TransactionID != "tx-2" {
		t.Fatalf("transaction id mismatch: got %q", proof.TransactionID)
	}
	root, err := MerkleRoot([][32]byte{SettlementLeaf(txs[0]), SettlementLeaf(txs[1])})
	if err != nil {
		t.Fatalf("MerkleRoot: %v", err)
	}
	if !VerifySettlementProof(root, txs[1], proof.Proof) {
		t.Fatal("proof did not verify against the transaction leaf")
	}
}

type stubSettlementSource struct {
	txs []*domain.Transaction
}

func (s *stubSettlementSource) GetByID(_ context.Context, id string) (*domain.Transaction, error) {
	for _, tx := range s.txs {
		if tx.ID == id {
			return tx, nil
		}
	}
	return nil, nil
}

func (s *stubSettlementSource) ListSettledBetween(_ context.Context, from, to time.Time) ([]*domain.Transaction, error) {
	out := make([]*domain.Transaction, 0)
	for _, tx := range s.txs {
		if tx.Status != domain.StatusSettled {
			continue
		}
		if !tx.CreatedAt.Before(from) && tx.CreatedAt.Before(to) {
			out = append(out, tx)
		}
	}
	return out, nil
}
