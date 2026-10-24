package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/protocols/horizon"
)

type balanceTestRepository struct {
	wallet *domain.Wallet
}

func (r *balanceTestRepository) Create(context.Context, *domain.Wallet) error { return nil }
func (r *balanceTestRepository) GetByID(context.Context, string) (*domain.Wallet, error) {
	return r.wallet, nil
}
func (r *balanceTestRepository) GetByPublicKey(context.Context, string) (*domain.Wallet, error) {
	return r.wallet, nil
}
func (r *balanceTestRepository) List(context.Context, int, int) ([]*domain.Wallet, error) {
	return []*domain.Wallet{r.wallet}, nil
}
func (r *balanceTestRepository) CountByTenant(context.Context, string) (int, error) { return 0, nil }
func (r *balanceTestRepository) UpsertBalance(context.Context, string, string, string, decimal.Decimal) error {
	return nil
}
func (r *balanceTestRepository) GetBalances(context.Context, string) ([]domain.BalanceRecord, error) {
	return []domain.BalanceRecord{}, nil
}
func (r *balanceTestRepository) UpdateSyncCursor(context.Context, string, string) error { return nil }

func TestServiceUsesAuthenticatedEnvironmentForWalletReads(t *testing.T) {
	repo := &balanceTestRepository{wallet: &domain.Wallet{ID: "wallet-1", Mode: domain.ModeTest}}
	svc := NewService(repo, nil, nil)
	ctx := tenant.WithID(context.Background(), "tenant-1")
	ctx = tenant.WithMode(ctx, domain.ModeTest)

	got, err := svc.GetWalletForHandler(ctx, "wallet-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != domain.ModeTest {
		t.Fatalf("wallet mode = %q, want test", got.Mode)
	}
}

func newTrustlineService(t *testing.T, balances []horizon.Balance) (Service, *walletTestClient, *walletTestRepo, *stubSigner) {
	t.Helper()
	walletPK := newTestKey(t)
	issuer := newTestKey(t)
	repo := newWalletTestRepo(&domain.Wallet{ID: "w-1", PublicKey: walletPK, EncryptedSecret: "secret"})
	client := &walletTestClient{accounts: map[string]horizon.Account{
		walletPK: {AccountID: walletPK, Balances: balances},
		issuer:   {AccountID: issuer, Flags: horizon.AccountFlags{AuthRequired: true, AuthRevocable: true}},
	}}
	signer := &stubSigner{}
	svc := NewService(repo, client, nil).WithSigner(signer)
	return svc, client, repo, signer
}

func TestEnsureTrustlineExistingIsNoop(t *testing.T) {
	issuer := newTestKey(t)
	svc, client, _, _ := newTrustlineService(t, []horizon.Balance{
		nativeBalanceEntry("10.0000000"),
		creditBalance("USDC", issuer, "1.0000000", "922337203685.4775807"),
	})

	hash, err := svc.(*service).EnsureTrustline(context.Background(), "w-1", "USDC", issuer, decimal.NewFromInt(1))
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" || len(client.submitted) != 0 {
		t.Fatalf("expected no ChangeTrust, hash=%q submissions=%d", hash, len(client.submitted))
	}
}

func TestEnsureTrustlineRejectsLowReserve(t *testing.T) {
	issuer := newTestKey(t)
	svc, client, _, _ := newTrustlineService(t, []horizon.Balance{nativeBalanceEntry("1.0000000")})

	_, err := svc.(*service).EnsureTrustline(context.Background(), "w-1", "USDC", issuer, decimal.NewFromInt(1))
	if !errors.Is(err, ErrTrustlineReserve) {
		t.Fatalf("err = %v, want ErrTrustlineReserve", err)
	}
	if len(client.submitted) != 0 {
		t.Fatal("must not submit without reserve")
	}
}

func TestEnsureTrustlineCreatesWhenMissing(t *testing.T) {
	issuer := newTestKey(t)
	svc, client, _, signer := newTrustlineService(t, []horizon.Balance{nativeBalanceEntry("10.0000000")})
	walletPK := repoWalletPublicKey(t, svc)
	client.onSubmit = func() {
		acct := client.accounts[walletPK]
		acct.Balances = append(acct.Balances, creditBalance("USDC", issuer, "0.0000000", "922337203685.4775807"))
		client.accounts[walletPK] = acct
	}

	hash, err := svc.(*service).EnsureTrustline(context.Background(), "w-1", "USDC", issuer, decimal.NewFromInt(1))
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" || len(client.submitted) != 1 {
		t.Fatalf("expected one ChangeTrust, hash=%q submissions=%d", hash, len(client.submitted))
	}
	if signer.calls != 1 {
		t.Fatalf("signer calls = %d, want 1", signer.calls)
	}
}

func TestEnsureTrustlineRejectsLimitOverflow(t *testing.T) {
	issuer := newTestKey(t)
	svc, client, _, _ := newTrustlineService(t, []horizon.Balance{
		nativeBalanceEntry("10.0000000"),
		creditBalance("USDC", issuer, "5.0000000", "10.0000000"),
	})

	_, err := svc.(*service).EnsureTrustline(context.Background(), "w-1", "USDC", issuer, decimal.NewFromInt(6))
	if !errors.Is(err, ErrTrustlineLimitExceeded) {
		t.Fatalf("err = %v, want ErrTrustlineLimitExceeded", err)
	}
	if len(client.submitted) != 0 {
		t.Fatal("must not submit when limit is exceeded")
	}
}

func TestEnsureTrustlineRejectsUnauthorizedAsset(t *testing.T) {
	issuer := newTestKey(t)
	unauthorized := creditBalance("USDC", issuer, "0.0000000", "922337203685.4775807")
	unauthorized.IsAuthorized = boolPtr(false)
	svc, _, _, _ := newTrustlineService(t, []horizon.Balance{
		nativeBalanceEntry("10.0000000"),
		unauthorized,
	})

	_, err := svc.(*service).EnsureTrustline(context.Background(), "w-1", "USDC", issuer, decimal.NewFromInt(1))
	if !errors.Is(err, ErrTrustlineUnauthorized) {
		t.Fatalf("err = %v, want ErrTrustlineUnauthorized", err)
	}
}

func TestTrustlineStatusDetectsSEP8(t *testing.T) {
	issuer := newTestKey(t)
	unauthorized := creditBalance("USDC", issuer, "0.0000000", "922337203685.4775807")
	unauthorized.IsAuthorized = boolPtr(false)
	svc, _, _, _ := newTrustlineService(t, []horizon.Balance{unauthorized})

	status, err := svc.(*service).TrustlineStatus(context.Background(), "w-1", "USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Exists || status.Authorized {
		t.Fatalf("unexpected status: %+v", status)
	}
	if !status.SEP8 || !status.IssuerAuthRequired || !status.IssuerAuthRevocable {
		t.Fatalf("SEP-8 flags not detected: %+v", status)
	}
}

func repoWalletPublicKey(t *testing.T, svc Service) string {
	t.Helper()
	w, err := svc.(*service).repo.GetByID(context.Background(), "w-1")
	if err != nil {
		t.Fatal(err)
	}
	return w.PublicKey
}
