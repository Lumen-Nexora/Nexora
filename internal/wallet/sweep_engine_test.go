package wallet

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/crypto"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/protocols/horizon/base"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/support/render/problem"
	"github.com/stellar/go/txnbuild"
)

const sweepTenant = "tenant-sweep"

var sweepMasterKey = make([]byte, 32)

// sweepStore is an in-memory SweepRepository.
type sweepStore struct {
	mu       sync.Mutex
	targets  []*domain.TenantCustodySettings
	wallets  []*domain.Wallet
	pending  map[string]int
	recorded []*domain.CustodialSweep
}

func (s *sweepStore) ListSweepTargets(context.Context) ([]*domain.TenantCustodySettings, error) {
	return s.targets, nil
}
func (s *sweepStore) ListCustodialWallets(_ context.Context, tenantID string, _ domain.Mode, afterID string, limit int) ([]*domain.Wallet, error) {
	var out []*domain.Wallet
	started := afterID == ""
	for _, w := range s.wallets {
		if !started {
			started = w.ID == afterID
			continue
		}
		if w.TenantID != nil && *w.TenantID == tenantID {
			out = append(out, w)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (s *sweepStore) CountPendingTransfers(_ context.Context, walletID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[walletID], nil
}
func (s *sweepStore) RecordSweep(_ context.Context, sweep *domain.CustodialSweep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sweep
	s.recorded = append(s.recorded, &cp)
	return nil
}

// sweepLedger serves accounts and captures every submitted transaction. It is
// safe for the engine's concurrent account loads.
type sweepLedger struct {
	mu        sync.Mutex
	accounts  map[string]horizon.Account
	submitted []*txnbuild.Transaction
	submitErr error
}

func (l *sweepLedger) LoadAccount(id string) (horizon.Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.accounts[id]; ok {
		return a, nil
	}
	return horizon.Account{}, &horizonclient.Error{Problem: problem.P{Status: 404}}
}
func (l *sweepLedger) SubmitTransaction(tx *txnbuild.Transaction) (horizon.Transaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.submitErr != nil {
		return horizon.Transaction{}, l.submitErr
	}
	l.submitted = append(l.submitted, tx)
	return horizon.Transaction{Hash: "sweep-hash"}, nil
}
func (l *sweepLedger) FindPathsStrict(string, string, string, string, string) ([]horizon.Path, error) {
	return nil, nil
}
func (l *sweepLedger) TransactionDetail(string) (horizon.Transaction, error) {
	return horizon.Transaction{}, nil
}
func (l *sweepLedger) OperationsForTransaction(string) ([]operations.Operation, error) {
	return nil, nil
}
func (l *sweepLedger) Payments(string, string, uint) ([]operations.Operation, error) {
	return nil, nil
}
func (l *sweepLedger) StreamPayments(context.Context, string, string, func(operations.Operation) error) error {
	return nil
}
func (l *sweepLedger) Offers(string, uint) ([]horizon.Offer, error) { return nil, nil }

type sweepEvents struct {
	mu       sync.Mutex
	payloads []map[string]interface{}
	tenants  []string
}

func (e *sweepEvents) Dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if eventType != domain.EventWalletSwept {
		return errors.New("unexpected event " + string(eventType))
	}
	e.payloads = append(e.payloads, payload.(map[string]interface{}))
	e.tenants = append(e.tenants, tenant.IDFromContext(ctx))
	return nil
}

type sweepFixture struct {
	engine *SweepEngine
	store  *sweepStore
	ledger *sweepLedger
	events *sweepEvents
	master string
	usdc   string
	keys   map[string]*keypair.Full
}

func newSweepFixture(t *testing.T, thresholds string) *sweepFixture {
	t.Helper()
	parsed, err := ParseSweepThresholds(thresholds)
	if err != nil {
		t.Fatal(err)
	}
	master := keypair.MustRandom().Address()
	usdc := keypair.MustRandom().Address()
	f := &sweepFixture{
		store: &sweepStore{
			targets: []*domain.TenantCustodySettings{{
				TenantID: sweepTenant, Mode: domain.ModeTest, MasterAddress: master, AutoSweepEnabled: true,
			}},
			pending: map[string]int{},
		},
		ledger: &sweepLedger{accounts: map[string]horizon.Account{
			master: {AccountID: master, Sequence: 1, Balances: []horizon.Balance{
				{Balance: "0", Limit: "922337203685.4775807", Asset: base.Asset{Type: "credit_alphanum4", Code: "USDC", Issuer: usdc}},
				{Balance: "500", Asset: base.Asset{Type: "native"}},
			}},
		}},
		events: &sweepEvents{},
		master: master,
		usdc:   usdc,
		keys:   map[string]*keypair.Full{},
	}
	f.engine = NewSweepEngine(f.store, f.ledger, stellar.NewEnvSigner(sweepMasterKey, "testnet"), f.events, SweepConfig{
		Thresholds: parsed, Concurrency: 4,
	})
	return f
}

// addWallet registers a custodial wallet with a real key, stored the way
// CreateWallet stores it, and its on-ledger account.
func (f *sweepFixture) addWallet(t *testing.T, id string, subentries int32, balances ...horizon.Balance) *domain.Wallet {
	t.Helper()
	kp := keypair.MustRandom()
	envelope, err := crypto.Encrypt([]byte(kp.Seed()), sweepMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := sweepTenant
	w := &domain.Wallet{
		ID: id, TenantID: &tenantID, PublicKey: kp.Address(), Mode: domain.ModeTest,
		CustodyType: domain.CustodyCustodial, EncryptedSecret: hex.EncodeToString(envelope),
	}
	f.store.wallets = append(f.store.wallets, w)
	f.ledger.accounts[kp.Address()] = horizon.Account{
		AccountID: kp.Address(), Sequence: 50, SubentryCount: subentries, Balances: balances,
	}
	f.keys[id] = kp
	return w
}

func native(balance string) horizon.Balance {
	return horizon.Balance{Balance: balance, Asset: base.Asset{Type: "native"}}
}

func (f *sweepFixture) credit(code, balance string) horizon.Balance {
	return horizon.Balance{Balance: balance, Limit: "1000000", Asset: base.Asset{Type: "credit_alphanum4", Code: code, Issuer: f.usdc}}
}

type sentPayment struct{ source, asset, amount string }

func payments(t *testing.T, tx *txnbuild.Transaction, master string) []sentPayment {
	t.Helper()
	var out []sentPayment
	for _, op := range tx.Operations() {
		p, ok := op.(*txnbuild.Payment)
		if !ok {
			t.Fatalf("unexpected operation %T", op)
		}
		if p.Destination != master {
			t.Fatalf("payment to %s, want master %s", p.Destination, master)
		}
		code := "XLM"
		if credit, ok := p.Asset.(txnbuild.CreditAsset); ok {
			code = credit.Code
		}
		out = append(out, sentPayment{source: p.SourceAccount, asset: code, amount: p.Amount})
	}
	return out
}

func TestMinimumBalance(t *testing.T) {
	half := decimal.RequireFromString("0.5")
	for _, tc := range []struct {
		name string
		acct horizon.Account
		want string
	}{
		{"bare account", horizon.Account{}, "1"},
		{"two trustlines", horizon.Account{SubentryCount: 2}, "2"},
		{"sponsoring adds", horizon.Account{SubentryCount: 1, NumSponsoring: 2}, "2.5"},
		{"sponsored entries are free", horizon.Account{SubentryCount: 2, NumSponsored: 2}, "1"},
	} {
		if got := MinimumBalance(tc.acct, half); !got.Equal(decimal.RequireFromString(tc.want)) {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseSweepThresholds(t *testing.T) {
	issuer := keypair.MustRandom().Address()
	got, err := ParseSweepThresholds(" xlm=25 , USDC:" + issuer + "=100,EURC=0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].AssetCode != "XLM" || got[1].Issuer != issuer || !got[2].Threshold.IsZero() {
		t.Fatalf("unexpected thresholds: %+v", got)
	}
	for _, bad := range []string{"XLM", "=5", "USDC=-1", "USDC=abc", "XLM:" + issuer + "=1", "USDC:not-a-key=1"} {
		if _, err := ParseSweepThresholds(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestSweep_BatchesExcessAboveReserveIntoOneTransaction(t *testing.T) {
	f := newSweepFixture(t, "XLM=10,USDC=100")
	w1 := f.addWallet(t, "w1", 1, f.credit("USDC", "500.0000000"), native("100.0000000"))
	w2 := f.addWallet(t, "w2", 0, native("50.0000000"))
	f.addWallet(t, "w3", 0, native("5.0000000")) // 4 XLM excess: below threshold

	report, err := f.engine.SweepAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Transactions != 1 || report.WalletsSwept != 2 || report.WalletsScanned != 3 || report.Skipped[SweepSkipBelowThreshold] != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(f.ledger.submitted) != 1 {
		t.Fatalf("want one batched transaction, got %d", len(f.ledger.submitted))
	}
	tx := f.ledger.submitted[0]

	// w1 has the most spare XLM, so it is the source and pays 3 x 100 stroops.
	if tx.SourceAccount().AccountID != w1.PublicKey || tx.BaseFee() != txnbuild.MinBaseFee {
		t.Fatalf("fee payer = %s, want w1", tx.SourceAccount().AccountID)
	}
	want := []sentPayment{
		// w1: min balance 1.5 (1 trustline); 98.5 excess less the 0.00003 fee.
		{w1.PublicKey, "USDC", "500.0000000"},
		{w1.PublicKey, "XLM", "98.4999700"},
		// w2: bare account keeps 1 XLM.
		{w2.PublicKey, "XLM", "49.0000000"},
	}
	got := payments(t, tx, f.master)
	if len(got) != len(want) {
		t.Fatalf("payments = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("payment %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Both wallets signed with their own decrypted keys.
	if _, err := stellar.VerifyTransactionSignatures(tx, network.TestNetworkPassphrase, []string{w1.PublicKey, w2.PublicKey}, 2); err != nil {
		t.Fatalf("signatures: %v", err)
	}

	if len(f.store.recorded) != 1 {
		t.Fatalf("want one recorded sweep, got %d", len(f.store.recorded))
	}
	rec := f.store.recorded[0]
	if rec.Status != domain.SweepStatusCompleted || rec.FeeStroops != 300 || *rec.TransactionHash != "sweep-hash" || len(rec.Items) != 3 {
		t.Fatalf("unexpected sweep record: %+v", rec)
	}
	// 100 - 98.49997 swept - 0.00003 fee leaves exactly the 1.5 XLM reserve.
	if rec.Items[1].Retained != "1.5000000" || rec.Items[1].BalanceBefore != "100.0000000" {
		t.Errorf("w1 should be left with exactly its reserve: %+v", rec.Items[1])
	}
}

func TestSweep_EmitsItemisedWalletSweptEvents(t *testing.T) {
	f := newSweepFixture(t, "XLM=10,USDC=100")
	w1 := f.addWallet(t, "w1", 1, f.credit("USDC", "500.0000000"), native("100.0000000"))
	w2 := f.addWallet(t, "w2", 0, native("50.0000000"))

	if _, err := f.engine.SweepAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.events.payloads) != 2 {
		t.Fatalf("want one wallet.swept per wallet, got %d", len(f.events.payloads))
	}
	for i, p := range f.events.payloads {
		if f.events.tenants[i] != sweepTenant {
			t.Errorf("event %d dispatched for tenant %q", i, f.events.tenants[i])
		}
		if p["transaction_hash"] != "sweep-hash" || p["destination_address"] != f.master || p["sweep_id"] == "" {
			t.Errorf("event %d missing sweep details: %+v", i, p)
		}
	}
	first, second := f.events.payloads[0], f.events.payloads[1]
	if first["wallet_id"] != w1.ID || len(first["items"].([]domain.SweepItem)) != 2 || first["fee_paid"] != "0.0000300" {
		t.Errorf("w1 event: %+v", first)
	}
	items := second["items"].([]domain.SweepItem)
	if second["wallet_id"] != w2.ID || len(items) != 1 || items[0].Amount != "49.0000000" || second["fee_paid"] != "0.0000000" {
		t.Errorf("w2 event: %+v", second)
	}
}

func TestSweep_PreservesLiabilitiesAndSponsorshipReserves(t *testing.T) {
	f := newSweepFixture(t, "XLM=1,USDC=1")
	usdc := f.credit("USDC", "300.0000000")
	usdc.SellingLiabilities = "100.0000000"
	xlm := native("40.0000000")
	xlm.SellingLiabilities = "4.0000000"
	w := f.addWallet(t, "w1", 2, usdc, xlm)
	acct := f.ledger.accounts[w.PublicKey]
	acct.NumSponsoring = 1
	f.ledger.accounts[w.PublicKey] = acct

	if _, err := f.engine.SweepAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := payments(t, f.ledger.submitted[0], f.master)
	// Min balance (2+2+1) x 0.5 = 2.5; minus 4 XLM liabilities; minus 0.00002 fee.
	if got[0] != (sentPayment{w.PublicKey, "USDC", "200.0000000"}) || got[1] != (sentPayment{w.PublicKey, "XLM", "33.4999800"}) {
		t.Fatalf("payments = %+v", got)
	}
}

func TestSweep_SkipsProtectedPendingUnfundedAndFrozen(t *testing.T) {
	f := newSweepFixture(t, "XLM=10,USDC=10")
	primary := f.addWallet(t, "primary", 0, native("100"))
	f.store.targets[0].PrimarySettlementWalletID = &primary.ID
	busy := f.addWallet(t, "busy", 0, native("100"))
	f.store.pending[busy.ID] = 1
	gone := f.addWallet(t, "gone", 0, native("100"))
	delete(f.ledger.accounts, gone.PublicKey)
	frozen := f.credit("USDC", "100")
	no := false
	frozen.IsAuthorized = &no
	f.addWallet(t, "frozen", 1, frozen, native("1.5"))
	// The master address itself is a custodial wallet of the tenant.
	f.store.wallets = append(f.store.wallets, &domain.Wallet{ID: "master", TenantID: primary.TenantID, PublicKey: f.master})

	report, err := f.engine.SweepAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ledger.submitted) != 0 {
		t.Fatalf("nothing should be swept, got %d transactions", len(f.ledger.submitted))
	}
	for reason, n := range map[string]int{
		SweepSkipProtected: 2, SweepSkipPending: 1, SweepSkipUnfunded: 1, SweepSkipBelowThreshold: 1,
	} {
		if report.Skipped[reason] != n {
			t.Errorf("skipped[%s] = %d, want %d (%+v)", reason, report.Skipped[reason], n, report.Skipped)
		}
	}
}

func TestSweep_OnlySendsAssetsTheMasterCanReceive(t *testing.T) {
	f := newSweepFixture(t, "XLM=10,USDC=10,EURC=10")
	// The master trusts USDC up to 150 more units, and does not trust EURC.
	master := f.ledger.accounts[f.master]
	master.Balances[0].Limit = "200"
	master.Balances[0].Balance = "50"
	f.ledger.accounts[f.master] = master
	a := f.addWallet(t, "a", 2, f.credit("USDC", "100"), f.credit("EURC", "100"), native("50"))
	b := f.addWallet(t, "b", 1, f.credit("USDC", "100"), native("50"))

	report, err := f.engine.SweepAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := payments(t, f.ledger.submitted[0], f.master)
	for _, p := range got {
		if p.asset == "EURC" || (p.source == b.PublicKey && p.asset == "USDC") {
			t.Fatalf("sent %+v the master cannot receive", p)
		}
	}
	if got[0] != (sentPayment{a.PublicKey, "USDC", "100.0000000"}) {
		t.Fatalf("payments = %+v", got)
	}
	if report.Skipped[SweepSkipNoTrustline] != 2 {
		t.Fatalf("want EURC and b's USDC skipped, got %+v", report.Skipped)
	}
}

func TestSweep_SplitsBatchesAtSignatureLimit(t *testing.T) {
	f := newSweepFixture(t, "XLM=1")
	for i := 0; i < 25; i++ {
		f.addWallet(t, "w"+string(rune('a'+i)), 0, native("10"))
	}
	report, err := f.engine.SweepAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Transactions != 2 || len(f.ledger.submitted[0].Operations()) != 20 || len(f.ledger.submitted[1].Operations()) != 5 {
		t.Fatalf("want 20+5 batches, report %+v", report)
	}
	if len(f.ledger.submitted[0].Signatures()) != 20 {
		t.Fatalf("want 20 signatures, got %d", len(f.ledger.submitted[0].Signatures()))
	}
}

func TestBatchSweeps_RespectsOperationLimit(t *testing.T) {
	plans := make([]*walletSweep, 17)
	for i := range plans {
		plans[i] = &walletSweep{payments: make([]sweepPayment, 6)}
	}
	batches := batchWallets(plans, maxSweepSignatures)
	if len(batches) != 2 || len(batches[0]) != 16 || len(batches[1]) != 1 {
		t.Fatalf("want 16 wallets (96 ops) then 1, got %d batches", len(batches))
	}
}

func TestSweep_RefusesBatchWhenNoWalletCanPayFee(t *testing.T) {
	f := newSweepFixture(t, "USDC=10")
	// Exactly the minimum balance: no spare XLM for the fee.
	f.addWallet(t, "w1", 1, f.credit("USDC", "100"), native("1.5"))

	report, err := f.engine.SweepAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ledger.submitted) != 0 || report.Skipped[SweepSkipFeeUnpayable] != 1 {
		t.Fatalf("batch should be refused, report %+v", report)
	}
}

func TestSweep_RecordsFailedSubmissionWithoutEvents(t *testing.T) {
	f := newSweepFixture(t, "XLM=10")
	f.addWallet(t, "w1", 0, native("100"))
	f.ledger.submitErr = errors.New("tx_bad_seq")

	report, err := f.engine.SweepAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.FailedTransactions != 1 || report.Transactions != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(f.store.recorded) != 1 || f.store.recorded[0].Status != domain.SweepStatusFailed || f.store.recorded[0].ErrorMessage == nil {
		t.Fatalf("failed sweep not recorded: %+v", f.store.recorded)
	}
	if len(f.events.payloads) != 0 {
		t.Fatal("wallet.swept must not be emitted for a failed sweep")
	}
}

func TestSweep_NoThresholdsIsANoop(t *testing.T) {
	f := newSweepFixture(t, "")
	f.addWallet(t, "w1", 0, native("100"))
	report, err := f.engine.SweepAll(context.Background())
	if err != nil || report.Targets != 0 || len(f.ledger.submitted) != 0 {
		t.Fatalf("expected a no-op, got %+v, %v", report, err)
	}
}

func TestSignWithStoredSecret_DecodesHexEnvelope(t *testing.T) {
	kp := keypair.MustRandom()
	envelope, err := crypto.Encrypt([]byte(kp.Seed()), sweepMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	source := horizon.Account{AccountID: kp.Address(), Sequence: 1}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &source, IncrementSequenceNum: true, BaseFee: txnbuild.MinBaseFee,
		Operations:    []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 5}},
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := stellar.NewEnvSigner(sweepMasterKey, "testnet")
	for name, stored := range map[string]string{
		"hex as stored by CreateWallet": hex.EncodeToString(envelope),
		"raw envelope":                  string(envelope),
	} {
		signed, err := signWithStoredSecret(signer, tx, stored)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := stellar.VerifyTransactionSignatures(signed, network.TestNetworkPassphrase, []string{kp.Address()}, 1); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
