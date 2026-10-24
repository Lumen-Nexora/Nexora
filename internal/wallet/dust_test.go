package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/protocols/horizon/base"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/txnbuild"
)

type walletTestRepo struct {
	wallets  []*domain.Wallet
	balances map[string][]domain.BalanceRecord
	upserts  map[string]decimal.Decimal
}

func newWalletTestRepo(wallets ...*domain.Wallet) *walletTestRepo {
	return &walletTestRepo{
		wallets:  wallets,
		balances: map[string][]domain.BalanceRecord{},
		upserts:  map[string]decimal.Decimal{},
	}
}

func (r *walletTestRepo) Create(context.Context, *domain.Wallet) error { return nil }
func (r *walletTestRepo) GetByID(_ context.Context, id string) (*domain.Wallet, error) {
	for _, w := range r.wallets {
		if w.ID == id {
			return w, nil
		}
	}
	return nil, domain.ErrWalletNotFound
}
func (r *walletTestRepo) GetByPublicKey(_ context.Context, pub string) (*domain.Wallet, error) {
	for _, w := range r.wallets {
		if w.PublicKey == pub {
			return w, nil
		}
	}
	return nil, domain.ErrWalletNotFound
}
func (r *walletTestRepo) List(_ context.Context, limit, offset int) ([]*domain.Wallet, error) {
	if offset >= len(r.wallets) {
		return nil, nil
	}
	end := offset + limit
	if end > len(r.wallets) {
		end = len(r.wallets)
	}
	return r.wallets[offset:end], nil
}
func (r *walletTestRepo) CountByTenant(context.Context, string) (int, error) {
	return len(r.wallets), nil
}
func (r *walletTestRepo) UpdateSyncCursor(context.Context, string, string) error { return nil }
func (r *walletTestRepo) UpsertBalance(_ context.Context, walletID, code, issuer string, balance decimal.Decimal) error {
	r.upserts[walletID+":"+code+":"+issuer] = balance
	return nil
}
func (r *walletTestRepo) GetBalances(_ context.Context, walletID string) ([]domain.BalanceRecord, error) {
	return r.balances[walletID], nil
}

type walletTestClient struct {
	accounts  map[string]horizon.Account
	errors    map[string]error
	submitErr error
	submitted []*txnbuild.Transaction
	onSubmit  func()
}

func (c *walletTestClient) LoadAccount(id string) (horizon.Account, error) {
	if err, ok := c.errors[id]; ok {
		return horizon.Account{}, err
	}
	acct, ok := c.accounts[id]
	if !ok {
		return horizon.Account{}, errors.New("account not found")
	}
	return acct, nil
}
func (c *walletTestClient) SubmitTransaction(tx *txnbuild.Transaction) (horizon.Transaction, error) {
	if c.submitErr != nil {
		return horizon.Transaction{}, c.submitErr
	}
	c.submitted = append(c.submitted, tx)
	if c.onSubmit != nil {
		c.onSubmit()
	}
	return horizon.Transaction{Hash: "dust-or-trust-tx"}, nil
}
func (c *walletTestClient) FindPathsStrict(string, string, string, string, string) ([]horizon.Path, error) {
	return nil, nil
}
func (c *walletTestClient) TransactionDetail(string) (horizon.Transaction, error) {
	return horizon.Transaction{}, nil
}
func (c *walletTestClient) OperationsForTransaction(string) ([]operations.Operation, error) {
	return nil, nil
}
func (c *walletTestClient) Payments(string, string, uint) ([]operations.Operation, error) {
	return nil, nil
}
func (c *walletTestClient) StreamPayments(context.Context, string, string, func(operations.Operation) error) error {
	return nil
}
func (c *walletTestClient) Offers(string, uint) ([]horizon.Offer, error) { return nil, nil }

type stubSigner struct{ calls int }

func (s *stubSigner) Sign(tx *txnbuild.Transaction, _ string) (*txnbuild.Transaction, error) {
	s.calls++
	return tx, nil
}

type stubFX struct{ rates map[string]decimal.Decimal }

func (f *stubFX) GetRates(_ context.Context, from, _ string) (*domain.RateResponse, error) {
	if rate, ok := f.rates[from]; ok {
		return &domain.RateResponse{Rate: rate}, nil
	}
	return nil, errors.New("no rate")
}

func boolPtr(v bool) *bool { return &v }

func newTestKey(t *testing.T) string {
	t.Helper()
	kp, err := keypair.Random()
	if err != nil {
		t.Fatal(err)
	}
	return kp.Address()
}

func creditBalance(code, issuer, amount, limit string) horizon.Balance {
	auth := true
	return horizon.Balance{
		Asset:        base.Asset{Type: "credit_alphanum4", Code: code, Issuer: issuer},
		Balance:      amount,
		Limit:        limit,
		IsAuthorized: &auth,
	}
}

func nativeBalanceEntry(amount string) horizon.Balance {
	return horizon.Balance{Asset: base.Asset{Type: "native", Code: "XLM"}, Balance: amount}
}

func TestScanDustClassifiesLowValueBalances(t *testing.T) {
	walletPK := newTestKey(t)
	issuer := newTestKey(t)
	repo := newWalletTestRepo(&domain.Wallet{ID: "w-1", PublicKey: walletPK, EncryptedSecret: "secret"})
	client := &walletTestClient{accounts: map[string]horizon.Account{
		walletPK: {AccountID: walletPK, Balances: []horizon.Balance{
			nativeBalanceEntry("10.0000000"),
			creditBalance("USDC", issuer, "0.0040000", "922337203685.4775807"),
			creditBalance("USDC", issuer, "5.0000000", "922337203685.4775807"),
		}},
	}}
	svc := NewService(repo, client, nil).WithFXService(&stubFX{rates: map[string]decimal.Decimal{
		"XLM": decimal.NewFromFloat(0.12),
	}})

	report, err := svc.(*service).ScanDust(context.Background(), DustConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.DustBalances) != 1 {
		t.Fatalf("expected 1 dust balance, got %d (%+v)", len(report.DustBalances), report.DustBalances)
	}
	if report.DustBalances[0].AssetCode != "USDC" || !report.DustBalances[0].Sweepable {
		t.Fatalf("unexpected dust classification: %+v", report.DustBalances[0])
	}
	if report.DustBalances[0].USDValue.String() != "0.004" {
		t.Fatalf("usd value = %s, want 0.004", report.DustBalances[0].USDValue)
	}
}

func TestScanDustSkipsUneconomicalSweeps(t *testing.T) {
	walletPK := newTestKey(t)
	issuer := newTestKey(t)
	repo := newWalletTestRepo(&domain.Wallet{ID: "w-1", PublicKey: walletPK})
	client := &walletTestClient{accounts: map[string]horizon.Account{
		walletPK: {AccountID: walletPK, Balances: []horizon.Balance{
			nativeBalanceEntry("10.0000000"),
			creditBalance("USDC", issuer, "0.0000001", "922337203685.4775807"),
		}},
	}}
	svc := NewService(repo, client, nil).WithFXService(&stubFX{rates: map[string]decimal.Decimal{
		"XLM": decimal.NewFromFloat(0.12),
	}})

	report, err := svc.(*service).ScanDust(context.Background(), DustConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if report.SkippedUneconomical != 1 {
		t.Fatalf("skipped = %d, want 1", report.SkippedUneconomical)
	}
	if len(report.DustBalances) != 1 || report.DustBalances[0].Sweepable {
		t.Fatalf("expected one non-sweepable dust balance, got %+v", report.DustBalances)
	}
}

func TestSweepDustConvertsAndUpdatesAccounting(t *testing.T) {
	walletPK := newTestKey(t)
	issuer := newTestKey(t)
	treasury := newTestKey(t)
	wallet := &domain.Wallet{ID: "w-1", PublicKey: walletPK, EncryptedSecret: "secret"}
	repo := newWalletTestRepo(wallet)
	client := &walletTestClient{accounts: map[string]horizon.Account{
		walletPK: {AccountID: walletPK, Balances: []horizon.Balance{
			nativeBalanceEntry("10.0000000"),
			creditBalance("USDC", issuer, "0.0050000", "922337203685.4775807"),
		}},
	}}
	signer := &stubSigner{}
	svc := NewService(repo, client, nil).
		WithSigner(signer).
		WithFXService(&stubFX{rates: map[string]decimal.Decimal{"XLM": decimal.NewFromFloat(0.12)}})

	report, err := svc.(*service).SweepDust(context.Background(), DustConfig{
		TreasuryAccount:     treasury,
		TreasuryAssetCode:   "USDC",
		TreasuryAssetIssuer: newTestKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sweeps) != 1 || len(client.submitted) != 1 {
		t.Fatalf("sweeps=%d submissions=%d", len(report.Sweeps), len(client.submitted))
	}
	op := client.submitted[0].Operations()[0]
	if _, ok := op.(*txnbuild.PathPaymentStrictSend); !ok {
		t.Fatalf("expected path payment, got %T", op)
	}
	if got := repo.upserts["w-1:USDC:"+issuer]; !got.IsZero() {
		t.Fatalf("source balance = %s, want 0", got)
	}
	if report.TotalDustCollected.String() != "0.005" {
		t.Fatalf("collected = %s, want 0.005", report.TotalDustCollected)
	}
	if report.TotalFeesConsumed.IsZero() {
		t.Fatal("expected fees to be accounted")
	}
}

func TestSweepDustBatchesOperations(t *testing.T) {
	walletPK := newTestKey(t)
	usdcIssuer := newTestKey(t)
	dustIssuer := newTestKey(t)
	treasury := newTestKey(t)
	repo := newWalletTestRepo(&domain.Wallet{ID: "w-1", PublicKey: walletPK, EncryptedSecret: "secret"})
	client := &walletTestClient{accounts: map[string]horizon.Account{
		walletPK: {AccountID: walletPK, Balances: []horizon.Balance{
			nativeBalanceEntry("10.0000000"),
			creditBalance("USDC", usdcIssuer, "0.0050000", "922337203685.4775807"),
			creditBalance("DUST", dustIssuer, "0.0040000", "922337203685.4775807"),
		}},
	}}
	svc := NewService(repo, client, nil).
		WithSigner(&stubSigner{}).
		WithFXService(&stubFX{rates: map[string]decimal.Decimal{"XLM": decimal.NewFromFloat(0.12)}})

	report, err := svc.(*service).SweepDust(context.Background(), DustConfig{
		TreasuryAccount:     treasury,
		TreasuryAssetCode:   "USDC",
		TreasuryAssetIssuer: usdcIssuer,
		MaxOpsPerTx:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.submitted) != 2 {
		t.Fatalf("submissions = %d, want 2 (one op per tx)", len(client.submitted))
	}
	if len(report.Sweeps) != 2 {
		t.Fatalf("sweeps = %d, want 2", len(report.Sweeps))
	}
}

func TestSweepDustDryRunDoesNotSubmit(t *testing.T) {
	walletPK := newTestKey(t)
	issuer := newTestKey(t)
	repo := newWalletTestRepo(&domain.Wallet{ID: "w-1", PublicKey: walletPK, EncryptedSecret: "secret"})
	client := &walletTestClient{accounts: map[string]horizon.Account{
		walletPK: {AccountID: walletPK, Balances: []horizon.Balance{
			nativeBalanceEntry("10.0000000"),
			creditBalance("USDC", issuer, "0.0050000", "922337203685.4775807"),
		}},
	}}
	svc := NewService(repo, client, nil).
		WithSigner(&stubSigner{}).
		WithFXService(&stubFX{rates: map[string]decimal.Decimal{"XLM": decimal.NewFromFloat(0.12)}})

	report, err := svc.(*service).SweepDust(context.Background(), DustConfig{
		TreasuryAccount: newTestKey(t),
		DryRun:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.submitted) != 0 || len(report.Sweeps) != 0 {
		t.Fatalf("dry run submitted %d transactions", len(client.submitted))
	}
	if len(report.DustBalances) != 1 {
		t.Fatalf("dry run should still report dust, got %+v", report.DustBalances)
	}
}
