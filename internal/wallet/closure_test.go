package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/protocols/horizon/base"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/support/render/problem"
	"github.com/stellar/go/txnbuild"
)

const closureTenant = "tenant-1"

// closureWallets is a tenant-aware in-memory Repository.
type closureWallets struct{ byID map[string]*domain.Wallet }

func (m *closureWallets) Create(_ context.Context, w *domain.Wallet) error {
	m.byID[w.ID] = w
	return nil
}
func (m *closureWallets) GetByID(_ context.Context, id string) (*domain.Wallet, error) {
	if w, ok := m.byID[id]; ok {
		return w, nil
	}
	return nil, domain.ErrWalletNotFound
}
func (m *closureWallets) GetByPublicKey(_ context.Context, pk string) (*domain.Wallet, error) {
	for _, w := range m.byID {
		if w.PublicKey == pk {
			return w, nil
		}
	}
	return nil, domain.ErrWalletNotFound
}
func (m *closureWallets) List(context.Context, int, int) ([]*domain.Wallet, error) { return nil, nil }
func (m *closureWallets) CountByTenant(context.Context, string) (int, error)       { return 0, nil }
func (m *closureWallets) UpdateSyncCursor(context.Context, string, string) error   { return nil }
func (m *closureWallets) UpsertBalance(context.Context, string, string, string, decimal.Decimal) error {
	return nil
}
func (m *closureWallets) GetBalances(context.Context, string) ([]domain.BalanceRecord, error) {
	return nil, nil
}

// closureStore is an in-memory ClosureRepository that enforces the
// one-live-closure-per-wallet index.
type closureStore struct {
	settings  *domain.TenantCustodySettings
	pending   int
	closures  map[string]*domain.AccountClosure
	createErr error
}

func (s *closureStore) Create(_ context.Context, c *domain.AccountClosure) error {
	if s.createErr != nil {
		return s.createErr
	}
	for _, existing := range s.closures {
		if existing.WalletID == c.WalletID && existing.Status != domain.ClosureStatusFailed {
			return domain.ErrClosureAlreadyExists
		}
	}
	c.ID = "closure-" + c.WalletID
	c.Status = domain.ClosureStatusPending
	cp := *c
	s.closures[c.ID] = &cp
	return nil
}
func (s *closureStore) Get(_ context.Context, id, _ string) (*domain.AccountClosure, error) {
	if c, ok := s.closures[id]; ok {
		return c, nil
	}
	return nil, domain.ErrAccountClosureNotFound
}
func (s *closureStore) List(context.Context, string, int, int) ([]*domain.AccountClosure, error) {
	return nil, nil
}
func (s *closureStore) Complete(_ context.Context, id, txHash string) error {
	s.closures[id].Status = domain.ClosureStatusCompleted
	s.closures[id].TransactionHash = &txHash
	return nil
}
func (s *closureStore) Fail(_ context.Context, id string, reason error) error {
	msg := reason.Error()
	s.closures[id].Status = domain.ClosureStatusFailed
	s.closures[id].ErrorMessage = &msg
	return nil
}
func (s *closureStore) CountPendingTransfers(context.Context, string) (int, error) {
	return s.pending, nil
}
func (s *closureStore) GetCustodySettings(context.Context, string, domain.Mode) (*domain.TenantCustodySettings, error) {
	if s.settings == nil {
		return nil, domain.ErrCustodySettingsNotFound
	}
	return s.settings, nil
}
func (s *closureStore) UpsertCustodySettings(_ context.Context, settings *domain.TenantCustodySettings) error {
	s.settings = settings
	return nil
}

// closureLedger serves accounts by ID and captures the submitted transaction.
type closureLedger struct {
	accounts  map[string]horizon.Account
	submitted *txnbuild.Transaction
	submitErr error
}

func (l *closureLedger) LoadAccount(id string) (horizon.Account, error) {
	if a, ok := l.accounts[id]; ok {
		return a, nil
	}
	return horizon.Account{}, &horizonclient.Error{Problem: problem.P{Status: 404}}
}
func (l *closureLedger) SubmitTransaction(tx *txnbuild.Transaction) (horizon.Transaction, error) {
	if l.submitErr != nil {
		return horizon.Transaction{}, l.submitErr
	}
	l.submitted = tx
	return horizon.Transaction{Hash: "merge-hash"}, nil
}
func (l *closureLedger) FindPathsStrict(string, string, string, string, string) ([]horizon.Path, error) {
	return nil, nil
}
func (l *closureLedger) TransactionDetail(string) (horizon.Transaction, error) {
	return horizon.Transaction{}, nil
}
func (l *closureLedger) OperationsForTransaction(string) ([]operations.Operation, error) {
	return nil, nil
}
func (l *closureLedger) Payments(string, string, uint) ([]operations.Operation, error) {
	return nil, nil
}
func (l *closureLedger) StreamPayments(context.Context, string, string, func(operations.Operation) error) error {
	return nil
}
func (l *closureLedger) Offers(string, uint) ([]horizon.Offer, error) { return nil, nil }

type passthroughSigner struct{ secret string }

func (s *passthroughSigner) Sign(tx *txnbuild.Transaction, encryptedSecret string) (*txnbuild.Transaction, error) {
	s.secret = encryptedSecret
	return tx, nil
}

type closureAudit struct {
	events []*domain.AuditEvent
	err    error
}

func (a *closureAudit) Record(_ context.Context, e *domain.AuditEvent) error {
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, e)
	return nil
}

type closureFixture struct {
	closer  *Closer
	wallets *closureWallets
	store   *closureStore
	ledger  *closureLedger
	audit   *closureAudit
	signer  *passthroughSigner
	wallet  *domain.Wallet
	master  string
	issuer  string
}

func newClosureFixture(t *testing.T) *closureFixture {
	t.Helper()
	tenantID := closureTenant
	walletKey := keypair.MustRandom().Address()
	master := keypair.MustRandom().Address()
	issuer := keypair.MustRandom().Address()

	w := &domain.Wallet{
		ID:              "w-close",
		TenantID:        &tenantID,
		PublicKey:       walletKey,
		EncryptedSecret: "enc-secret",
		CustodyType:     domain.CustodyCustodial,
	}
	primary := "w-primary"
	f := &closureFixture{
		wallets: &closureWallets{byID: map[string]*domain.Wallet{w.ID: w}},
		store: &closureStore{
			settings: &domain.TenantCustodySettings{
				TenantID:                  tenantID,
				MasterAddress:             master,
				PrimarySettlementWalletID: &primary,
			},
			closures: map[string]*domain.AccountClosure{},
		},
		ledger: &closureLedger{accounts: map[string]horizon.Account{
			walletKey: {
				AccountID:     walletKey,
				Sequence:      100,
				SubentryCount: 2,
				Balances: []horizon.Balance{
					{Balance: "0.0000000", Asset: base.Asset{Type: "credit_alphanum4", Code: "USDC", Issuer: issuer}},
					{Balance: "0.0000000", Asset: base.Asset{Type: "credit_alphanum4", Code: "EURC", Issuer: issuer}},
					{Balance: "2.5000000", Asset: base.Asset{Type: "native"}},
				},
				Signers: []horizon.Signer{{Key: walletKey, Weight: 1}},
			},
			master: {AccountID: master, Sequence: 1},
		}},
		audit:  &closureAudit{},
		signer: &passthroughSigner{},
		wallet: w,
		master: master,
		issuer: issuer,
	}
	f.closer = NewCloser(f.wallets, f.store, f.ledger, f.signer, f.audit)
	return f
}

func operatorCtx() context.Context {
	ctx := tenant.WithID(context.Background(), closureTenant)
	return tenant.WithUser(ctx, "operator-7", "owner")
}

func (f *closureFixture) request() CloseRequest {
	return CloseRequest{
		WalletID:      f.wallet.ID,
		Justification: "Customer offboarded; account inactive for 180 days",
		Confirm:       f.wallet.PublicKey,
	}
}

func (f *closureFixture) setAccount(mutate func(*horizon.Account)) {
	acct := f.ledger.accounts[f.wallet.PublicKey]
	mutate(&acct)
	f.ledger.accounts[f.wallet.PublicKey] = acct
}

func TestClose_RemovesTrustlinesThenMergesToMaster(t *testing.T) {
	f := newClosureFixture(t)

	closure, err := f.closer.Close(operatorCtx(), f.request())
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closure.Status != domain.ClosureStatusCompleted || closure.TransactionHash == nil || *closure.TransactionHash != "merge-hash" {
		t.Fatalf("closure not completed with hash: %+v", closure)
	}

	ops := f.ledger.submitted.Operations()
	if len(ops) != 3 {
		t.Fatalf("want 2 ChangeTrust + 1 AccountMerge, got %d ops", len(ops))
	}
	for i, op := range ops[:2] {
		ct, ok := op.(*txnbuild.ChangeTrust)
		if !ok {
			t.Fatalf("op %d is %T, want ChangeTrust before the merge", i, op)
		}
		if ct.Limit != "0" {
			t.Fatalf("op %d limit %q, want 0 to remove the trustline", i, ct.Limit)
		}
	}
	merge, ok := ops[2].(*txnbuild.AccountMerge)
	if !ok {
		t.Fatalf("last op is %T, want AccountMerge", ops[2])
	}
	if merge.Destination != f.master {
		t.Fatalf("merge destination %s, want tenant master %s", merge.Destination, f.master)
	}
	if f.signer.secret != "enc-secret" {
		t.Fatal("transaction was not signed with the wallet's stored secret")
	}
	if len(closure.RemovedTrustlines) != 2 || !closure.NativeBalance.Equal(decimal.RequireFromString("2.5")) {
		t.Fatalf("closure did not record trustlines and native balance: %+v", closure)
	}
}

func TestClose_RecordsComplianceAuditWithOperatorAndJustification(t *testing.T) {
	f := newClosureFixture(t)

	if _, err := f.closer.Close(operatorCtx(), f.request()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(f.audit.events) != 2 {
		t.Fatalf("want initiated + completed audit events, got %d", len(f.audit.events))
	}
	if f.audit.events[0].Action != AuditActionClosureInitiated || f.audit.events[1].Action != AuditActionClosureCompleted {
		t.Fatalf("unexpected audit actions: %s, %s", f.audit.events[0].Action, f.audit.events[1].Action)
	}
	for _, e := range f.audit.events {
		if e.ActorType != "user" || e.ActorID != "operator-7" {
			t.Fatalf("audit actor %s/%s, want user/operator-7", e.ActorType, e.ActorID)
		}
		if e.Metadata["operator_id"] != "operator-7" || e.Metadata["justification"] != f.request().Justification {
			t.Fatalf("audit metadata missing operator or justification: %v", e.Metadata)
		}
	}
	if f.audit.events[1].Metadata["transaction_hash"] != "merge-hash" {
		t.Fatal("completion audit event does not carry the transaction hash")
	}
}

func TestClose_FailsClosedWhenAuditCannotBeWritten(t *testing.T) {
	f := newClosureFixture(t)
	f.audit.err = errors.New("audit store down")

	closure, err := f.closer.Close(operatorCtx(), f.request())
	if err == nil {
		t.Fatal("closure proceeded without a compliance record")
	}
	if f.ledger.submitted != nil {
		t.Fatal("AccountMerge was submitted although the audit write failed")
	}
	if closure == nil || f.store.closures[closure.ID].Status != domain.ClosureStatusFailed {
		t.Fatal("closure record not marked failed")
	}
}

func TestClose_RefusesOutstandingNonNativeBalance(t *testing.T) {
	cases := map[string]func(*horizon.Balance){
		"balance":             func(b *horizon.Balance) { b.Balance = "0.0000001" },
		"buying liabilities":  func(b *horizon.Balance) { b.BuyingLiabilities = "5.0000000" },
		"selling liabilities": func(b *horizon.Balance) { b.SellingLiabilities = "1.0000000" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newClosureFixture(t)
			f.setAccount(func(a *horizon.Account) { mutate(&a.Balances[0]) })

			_, err := f.closer.Close(operatorCtx(), f.request())
			if !errors.Is(err, domain.ErrClosureOutstandingBalance) {
				t.Fatalf("want ErrClosureOutstandingBalance, got %v", err)
			}
			if f.ledger.submitted != nil || len(f.store.closures) != 0 {
				t.Fatal("refused closure wrote state or reached the ledger")
			}
		})
	}
}

func TestClose_RefusesPrimarySettlementAccount(t *testing.T) {
	f := newClosureFixture(t)
	id := f.wallet.ID
	f.store.settings.PrimarySettlementWalletID = &id

	if _, err := f.closer.Close(operatorCtx(), f.request()); !errors.Is(err, domain.ErrClosurePrimarySettlement) {
		t.Fatalf("want ErrClosurePrimarySettlement, got %v", err)
	}
}

func TestClose_RefusesMasterAccount(t *testing.T) {
	f := newClosureFixture(t)
	f.store.settings.MasterAddress = f.wallet.PublicKey

	if _, err := f.closer.Close(operatorCtx(), f.request()); !errors.Is(err, domain.ErrClosureMasterAccount) {
		t.Fatalf("want ErrClosureMasterAccount, got %v", err)
	}
}

func TestClose_RefusesPendingTransfers(t *testing.T) {
	f := newClosureFixture(t)
	f.store.pending = 1

	if _, err := f.closer.Close(operatorCtx(), f.request()); !errors.Is(err, domain.ErrClosurePendingTransfers) {
		t.Fatalf("want ErrClosurePendingTransfers, got %v", err)
	}
	if f.ledger.submitted != nil {
		t.Fatal("closure reached the ledger with pending transfers")
	}
}

func TestClose_RequestValidation(t *testing.T) {
	cases := []struct {
		name   string
		ctx    func() context.Context
		mutate func(*CloseRequest, *closureFixture)
		want   error
	}{
		{"no operator", func() context.Context { return tenant.WithID(context.Background(), closureTenant) }, nil, domain.ErrClosureOperatorRequired},
		{"short justification", operatorCtx, func(r *CloseRequest, _ *closureFixture) { r.Justification = "cleanup" }, domain.ErrClosureJustificationRequired},
		{"wrong confirmation", operatorCtx, func(r *CloseRequest, _ *closureFixture) { r.Confirm = "GWRONG" }, domain.ErrClosureConfirmationMismatch},
		{"foreign wallet", func() context.Context {
			return tenant.WithUser(tenant.WithID(context.Background(), "tenant-2"), "op", "owner")
		}, nil, domain.ErrWalletNotFound},
		{"contract wallet", operatorCtx, func(_ *CloseRequest, f *closureFixture) { f.wallet.CustodyType = domain.CustodyContract }, domain.ErrClosureNotCustodial},
		{"no custody settings", operatorCtx, func(_ *CloseRequest, f *closureFixture) { f.store.settings = nil }, domain.ErrCustodySettingsNotFound},
		{"unfunded master", operatorCtx, func(_ *CloseRequest, f *closureFixture) { delete(f.ledger.accounts, f.master) }, domain.ErrClosureMasterAddressNotFunded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newClosureFixture(t)
			req := f.request()
			if tc.mutate != nil {
				tc.mutate(&req, f)
			}
			if _, err := f.closer.Close(tc.ctx(), req); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if f.ledger.submitted != nil {
				t.Fatal("invalid request reached the ledger")
			}
		})
	}
}

func TestClose_RefusesSubentriesThatBlockMerge(t *testing.T) {
	cases := map[string]func(*horizon.Account){
		"open offer": func(a *horizon.Account) { a.SubentryCount = 3 },
		"data entry": func(a *horizon.Account) { a.Data = map[string]string{"k": "dg=="} },
		"extra signer": func(a *horizon.Account) {
			a.Signers = append(a.Signers, horizon.Signer{Key: keypair.MustRandom().Address(), Weight: 1})
		},
		"sponsoring":       func(a *horizon.Account) { a.NumSponsoring = 1 },
		"native liability": func(a *horizon.Account) { a.Balances[2].SellingLiabilities = "1.0000000" },
		"pool share": func(a *horizon.Account) {
			a.Balances[0] = horizon.Balance{Balance: "0", LiquidityPoolId: "abcd", Asset: base.Asset{Type: "liquidity_pool_shares"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newClosureFixture(t)
			f.setAccount(mutate)
			if _, err := f.closer.Close(operatorCtx(), f.request()); !errors.Is(err, domain.ErrClosureUnsupportedAccount) {
				t.Fatalf("want ErrClosureUnsupportedAccount, got %v", err)
			}
		})
	}
}

func TestClose_RefusesSecondClosure(t *testing.T) {
	f := newClosureFixture(t)
	if _, err := f.closer.Close(operatorCtx(), f.request()); err != nil {
		t.Fatalf("first close: %v", err)
	}
	f.ledger.submitted = nil
	if _, err := f.closer.Close(operatorCtx(), f.request()); !errors.Is(err, domain.ErrClosureAlreadyExists) {
		t.Fatalf("want ErrClosureAlreadyExists, got %v", err)
	}
	if f.ledger.submitted != nil {
		t.Fatal("second closure reached the ledger")
	}
}

func TestClose_SubmissionFailureMarksClosureFailed(t *testing.T) {
	f := newClosureFixture(t)
	f.ledger.submitErr = errors.New("tx_bad_seq")

	closure, err := f.closer.Close(operatorCtx(), f.request())
	if !errors.Is(err, domain.ErrStellarSubmission) {
		t.Fatalf("want ErrStellarSubmission, got %v", err)
	}
	if closure.Status != domain.ClosureStatusFailed || f.store.closures[closure.ID].Status != domain.ClosureStatusFailed {
		t.Fatal("failed submission not recorded on the closure")
	}
	last := f.audit.events[len(f.audit.events)-1]
	if last.Action != AuditActionClosureFailed {
		t.Fatalf("last audit action %s, want %s", last.Action, AuditActionClosureFailed)
	}
}

func TestSetCustodySettings(t *testing.T) {
	f := newClosureFixture(t)
	f.store.settings = nil

	if _, err := f.closer.SetCustodySettings(operatorCtx(), CustodySettingsInput{MasterAddress: "not-an-address"}); !errors.Is(err, domain.ErrInvalidMasterAddress) {
		t.Fatalf("want ErrInvalidMasterAddress, got %v", err)
	}
	foreign := "w-unknown"
	if _, err := f.closer.SetCustodySettings(operatorCtx(), CustodySettingsInput{MasterAddress: f.master, PrimarySettlementWalletID: &foreign}); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Fatalf("want ErrWalletNotFound for unknown settlement wallet, got %v", err)
	}

	settings, err := f.closer.SetCustodySettings(operatorCtx(), CustodySettingsInput{MasterAddress: f.master, PrimarySettlementWalletID: &f.wallet.ID, AutoSweepEnabled: true})
	if err != nil {
		t.Fatalf("set settings: %v", err)
	}
	if settings.UpdatedBy != "operator-7" || settings.Mode != domain.ModeLive || !settings.AutoSweepEnabled || f.store.settings != settings {
		t.Fatalf("settings not stored with operator and mode: %+v", settings)
	}
	if len(f.audit.events) != 1 || f.audit.events[0].Action != AuditActionCustodySettings {
		t.Fatal("custody settings change was not audited")
	}
}
