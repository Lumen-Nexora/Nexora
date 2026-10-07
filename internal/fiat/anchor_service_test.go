package fiat

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/crypto"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
)

type fakeWalletGetter struct {
	wallet *domain.Wallet
}

func (f *fakeWalletGetter) GetByID(ctx context.Context, id string) (*domain.Wallet, error) {
	return f.wallet, nil
}

type fakeAnchorRepo struct {
	mu  sync.Mutex
	txs map[string]*domain.AnchorTransaction
}

func newFakeAnchorRepo() *fakeAnchorRepo {
	return &fakeAnchorRepo{txs: make(map[string]*domain.AnchorTransaction)}
}

func (f *fakeAnchorRepo) CreateTransaction(ctx context.Context, t *domain.AnchorTransaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *t
	f.txs[t.ID] = &cp
	return nil
}

func (f *fakeAnchorRepo) GetTransactionByID(ctx context.Context, id string, tenantID *string) (*domain.AnchorTransaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.txs[id]
	if !ok {
		return nil, domain.ErrTransactionNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeAnchorRepo) UpdateTransactionStatus(ctx context.Context, id, status string, completedAt *time.Time, tenantID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.txs[id]
	if !ok {
		return domain.ErrTransactionNotFound
	}
	t.Status = status
	t.CompletedAt = completedAt
	return nil
}

type fakeAnchorRegistry struct {
	a *domain.Anchor
}

func (f *fakeAnchorRegistry) ResolveForAsset(assetCode string) (*domain.Anchor, error) {
	return f.a, nil
}

func (f *fakeAnchorRegistry) GetByID(id string) (*domain.Anchor, error) {
	return f.a, nil
}

// buildSignedChallenge mirrors what a real anchor's GET /web_auth does: build
// a SEP-10 challenge transaction sourced from and signed by serverKP.
func buildSignedChallenge(t *testing.T, serverKP, clientKP *keypair.Full, webAuthDomain, homeDomain string) string {
	t.Helper()
	tx, err := txnbuild.BuildChallengeTx(serverKP.Seed(), clientKP.Address(), webAuthDomain, homeDomain, network.TestNetworkPassphrase, 300*time.Second, nil)
	if err != nil {
		t.Fatalf("build challenge: %v", err)
	}
	xdrStr, err := tx.Base64()
	if err != nil {
		t.Fatalf("encode challenge: %v", err)
	}
	return xdrStr
}

// newFakeAnchorServer stands in for a real SEP-10 + SEP-6 compliant anchor:
// GET/POST /auth for SEP-10, GET /deposit and GET /transaction for SEP-6.
// transactionStatus controls what GET /transaction reports, so tests can
// simulate the anchor confirming a deposit on-chain.
func newFakeAnchorServer(t *testing.T, serverKP, clientKP *keypair.Full, homeDomain string, transactionStatus *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			challenge := buildSignedChallenge(t, serverKP, clientKP, r.Host, homeDomain)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"transaction":        challenge,
				"network_passphrase": network.TestNetworkPassphrase,
			})
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "sep10-jwt"})
		}
	})
	mux.HandleFunc("/deposit", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":  "deposit-tx-1",
			"how": "wire to Test Bank",
			"instructions": map[string]interface{}{
				"organization.bank_number": map[string]string{"value": "111000025", "description": "routing number"},
			},
		})
	})
	mux.HandleFunc("/transaction", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"transaction": map[string]interface{}{
				"id":     "deposit-tx-1",
				"status": *transactionStatus,
			},
		})
	})
	return httptest.NewServer(mux)
}

func TestAnchorFiatService_InitiateDeposit_And_PollUntilCompleted(t *testing.T) {
	serverKP := keypair.MustRandom()
	walletKP := keypair.MustRandom()

	status := "pending_user_transfer_start"
	srv := newFakeAnchorServer(t, serverKP, walletKP, "test.anchor.example.com", &status)
	defer srv.Close()

	a := &domain.Anchor{
		ID:                "anchor-1",
		HomeDomain:        "test.anchor.example.com",
		TransferServer:    srv.URL,
		WebAuthEndpoint:   srv.URL + "/auth",
		Sep10SigningKey:   serverKP.Address(),
		NetworkPassphrase: network.TestNetworkPassphrase,
		SupportedAssets:   []string{"USDC"},
	}

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}
	encrypted, err := crypto.Encrypt([]byte(walletKP.Seed()), masterKey)
	if err != nil {
		t.Fatalf("encrypt wallet secret: %v", err)
	}
	wallet := &domain.Wallet{ID: "wallet-1", PublicKey: walletKP.Address(), EncryptedSecret: hex.EncodeToString(encrypted)}

	repo := newFakeAnchorRepo()
	svc := NewAnchorFiatService(&fakeAnchorRegistry{a: a}, repo, &fakeWalletGetter{wallet: wallet}, masterKey, "testnet")

	res, err := svc.InitiateDeposit(context.Background(), AnchorDepositRequest{
		WalletID: "wallet-1", AssetCode: "USDC", Amount: "100", Email: "user@example.com",
	})
	if err != nil {
		t.Fatalf("InitiateDeposit returned unexpected error: %v", err)
	}
	if res.Type != "sep6" {
		t.Fatalf("expected sep6 flow, got %q", res.Type)
	}
	if res.Instructions == nil || res.Instructions.ID != "deposit-tx-1" {
		t.Fatalf("expected deposit instructions with id deposit-tx-1, got %+v", res.Instructions)
	}
	if res.TransactionID == "" {
		t.Fatalf("expected a transaction id to be assigned")
	}

	// First poll: anchor still says pending.
	tx, err := svc.GetTransaction(context.Background(), res.TransactionID)
	if err != nil {
		t.Fatalf("GetTransaction returned unexpected error: %v", err)
	}
	if tx.Status != "pending_user_transfer_start" {
		t.Fatalf("expected pending status, got %q", tx.Status)
	}
	if tx.CompletedAt != nil {
		t.Fatalf("expected no completed_at while pending")
	}

	// The anchor confirms the deposit on-chain.
	status = "completed"

	tx, err = svc.GetTransaction(context.Background(), res.TransactionID)
	if err != nil {
		t.Fatalf("GetTransaction returned unexpected error: %v", err)
	}
	if tx.Status != "completed" {
		t.Fatalf("expected completed status after anchor confirms deposit, got %q", tx.Status)
	}
	if tx.CompletedAt == nil {
		t.Fatalf("expected completed_at to be set once status is completed")
	}
}
