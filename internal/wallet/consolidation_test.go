package wallet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/crypto"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/queue"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/hibiken/asynq"
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

const consolidationTenant = "tenant-consolidate"

// ── mock testnet ─────────────────────────────────────────────────────────────

type testnetLine struct {
	balance decimal.Decimal
	limit   decimal.Decimal
}

type testnetAccount struct {
	seq    int64
	native decimal.Decimal
	lines  map[string]*testnetLine // "CODE:ISSUER"
	order  []string
}

func (a *testnetAccount) clone() *testnetAccount {
	cp := &testnetAccount{seq: a.seq, native: a.native, lines: map[string]*testnetLine{}, order: append([]string(nil), a.order...)}
	for k, l := range a.lines {
		line := *l
		cp.lines[k] = &line
	}
	return cp
}

// testnetLedger is an in-memory Stellar network: it serves accounts the way
// Horizon does and applies submitted transactions with the rules that matter
// here: sequence numbers, a valid signature from every operation's source,
// fees, trustlines and limits, minimum balances, and all-or-nothing
// application.
type testnetLedger struct {
	mu        sync.Mutex
	accounts  map[string]*testnetAccount
	submitted int
	failOn    map[int]bool // 1-based submission numbers to reject
}

func newTestnetLedger() *testnetLedger {
	return &testnetLedger{accounts: map[string]*testnetAccount{}, failOn: map[int]bool{}}
}

func (l *testnetLedger) fund(address, native string, lines ...[3]string) {
	acct := &testnetAccount{seq: 100, native: decimal.RequireFromString(native), lines: map[string]*testnetLine{}}
	for _, line := range lines { // {key, balance, limit}
		acct.lines[line[0]] = &testnetLine{balance: decimal.RequireFromString(line[1]), limit: decimal.RequireFromString(line[2])}
		acct.order = append(acct.order, line[0])
	}
	l.accounts[address] = acct
}

func (l *testnetLedger) LoadAccount(id string) (horizon.Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.accounts[id]
	if !ok {
		return horizon.Account{}, &horizonclient.Error{Problem: problem.P{Status: 404}}
	}
	out := horizon.Account{AccountID: id, Sequence: a.seq, SubentryCount: int32(len(a.lines)),
		Signers: []horizon.Signer{{Key: id, Weight: 1}}}
	for _, key := range a.order {
		line, ok := a.lines[key]
		if !ok {
			continue
		}
		code, issuer := splitKey(key)
		out.Balances = append(out.Balances, horizon.Balance{
			Balance: line.balance.StringFixed(7), Limit: line.limit.StringFixed(7),
			Asset: base.Asset{Type: "credit_alphanum4", Code: code, Issuer: issuer},
		})
	}
	out.Balances = append(out.Balances, horizon.Balance{Balance: a.native.StringFixed(7), Asset: base.Asset{Type: "native"}})
	return out, nil
}

func splitKey(key string) (string, string) {
	for i := range key {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

func (l *testnetLedger) SubmitTransaction(tx *txnbuild.Transaction) (horizon.Transaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.submitted++
	if l.failOn[l.submitted] {
		return horizon.Transaction{}, errors.New("tx_failed")
	}

	sourceID := tx.SourceAccount().AccountID
	state := map[string]*testnetAccount{}
	get := func(id string) (*testnetAccount, error) {
		if a, ok := state[id]; ok {
			return a, nil
		}
		a, ok := l.accounts[id]
		if !ok {
			return nil, fmt.Errorf("account %s not found", id)
		}
		state[id] = a.clone()
		return state[id], nil
	}
	src, err := get(sourceID)
	if err != nil {
		return horizon.Transaction{}, err
	}
	if tx.SourceAccount().Sequence != src.seq+1 {
		return horizon.Transaction{}, errors.New("tx_bad_seq")
	}

	required := map[string]bool{sourceID: true}
	for _, op := range tx.Operations() {
		if s := op.GetSourceAccount(); s != "" {
			required[s] = true
		}
	}
	var signers []string
	for k := range required {
		signers = append(signers, k)
	}
	if len(tx.Signatures()) > 20 {
		return horizon.Transaction{}, errors.New("tx_too_many_signatures")
	}
	if _, err := stellar.VerifyTransactionSignatures(tx, network.TestNetworkPassphrase, signers, len(signers)); err != nil {
		return horizon.Transaction{}, fmt.Errorf("tx_bad_auth: %w", err)
	}

	src.seq++
	src.native = src.native.Sub(decimal.New(tx.MaxFee(), -7))
	merged := map[string]bool{}
	for _, op := range tx.Operations() {
		opSource := op.GetSourceAccount()
		if opSource == "" {
			opSource = sourceID
		}
		from, err := get(opSource)
		if err != nil || merged[opSource] {
			return horizon.Transaction{}, errors.New("op_no_source_account")
		}
		switch o := op.(type) {
		case *txnbuild.Payment:
			to, err := get(o.Destination)
			if err != nil {
				return horizon.Transaction{}, errors.New("op_no_destination")
			}
			amount := decimal.RequireFromString(o.Amount)
			if _, ok := o.Asset.(txnbuild.NativeAsset); ok {
				from.native = from.native.Sub(amount)
				to.native = to.native.Add(amount)
				continue
			}
			credit := o.Asset.(txnbuild.CreditAsset)
			key := credit.Code + ":" + credit.Issuer
			fromLine, toLine := from.lines[key], to.lines[key]
			if toLine == nil {
				return horizon.Transaction{}, errors.New("op_no_trust")
			}
			if fromLine == nil || fromLine.balance.LessThan(amount) {
				return horizon.Transaction{}, errors.New("op_underfunded")
			}
			if toLine.balance.Add(amount).GreaterThan(toLine.limit) {
				return horizon.Transaction{}, errors.New("op_line_full")
			}
			fromLine.balance = fromLine.balance.Sub(amount)
			toLine.balance = toLine.balance.Add(amount)
		case *txnbuild.ChangeTrust:
			asset, _ := o.Line.ToAsset()
			key := asset.GetCode() + ":" + asset.GetIssuer()
			line := from.lines[key]
			if o.Limit != "0" || line == nil || !line.balance.IsZero() {
				return horizon.Transaction{}, errors.New("op_invalid_limit")
			}
			delete(from.lines, key)
		case *txnbuild.AccountMerge:
			if len(from.lines) > 0 {
				return horizon.Transaction{}, errors.New("op_has_sub_entries")
			}
			to, err := get(o.Destination)
			if err != nil {
				return horizon.Transaction{}, errors.New("op_no_destination")
			}
			to.native = to.native.Add(from.native)
			from.native = decimal.Zero
			merged[opSource] = true
		default:
			return horizon.Transaction{}, fmt.Errorf("unsupported operation %T", op)
		}
	}
	for id, a := range state {
		if merged[id] {
			continue
		}
		min := decimal.NewFromFloat(0.5).Mul(decimal.NewFromInt(int64(2 + len(a.lines))))
		if a.native.LessThan(min) {
			return horizon.Transaction{}, fmt.Errorf("op_underfunded: %s below minimum balance", id)
		}
	}
	for id, a := range state {
		if merged[id] {
			delete(l.accounts, id)
		} else {
			l.accounts[id] = a
		}
	}
	return horizon.Transaction{Hash: "tx-" + strconv.Itoa(l.submitted)}, nil
}

func (l *testnetLedger) FindPathsStrict(string, string, string, string, string) ([]horizon.Path, error) {
	return nil, nil
}
func (l *testnetLedger) TransactionDetail(string) (horizon.Transaction, error) {
	return horizon.Transaction{}, nil
}
func (l *testnetLedger) OperationsForTransaction(string) ([]operations.Operation, error) {
	return nil, nil
}
func (l *testnetLedger) Payments(string, string, uint) ([]operations.Operation, error) {
	return nil, nil
}
func (l *testnetLedger) StreamPayments(context.Context, string, string, func(operations.Operation) error) error {
	return nil
}
func (l *testnetLedger) Offers(string, uint) ([]horizon.Offer, error) { return nil, nil }

func (l *testnetLedger) native(address string) decimal.Decimal {
	return l.accounts[address].native
}

func (l *testnetLedger) credit(address, key string) decimal.Decimal {
	return l.accounts[address].lines[key].balance
}

// ── in-memory stores ─────────────────────────────────────────────────────────

type consolidationStore struct {
	mu  sync.Mutex
	ops map[string]*domain.ConsolidationOperation
	seq int
}

func (s *consolidationStore) Create(_ context.Context, op *domain.ConsolidationOperation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.ops {
		if existing.TenantID == op.TenantID && existing.IdempotencyKey == op.IdempotencyKey {
			return domain.ErrConsolidationKeyConflict
		}
	}
	s.seq++
	op.ID = "cons-" + strconv.Itoa(s.seq)
	op.Status = domain.ConsolidationPending
	cp := *op
	s.ops[op.ID] = &cp
	return nil
}
func (s *consolidationStore) Get(_ context.Context, id, tenantID string) (*domain.ConsolidationOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[id]; ok && op.TenantID == tenantID {
		cp := *op
		return &cp, nil
	}
	return nil, domain.ErrConsolidationNotFound
}
func (s *consolidationStore) GetByIdempotencyKey(_ context.Context, tenantID, key string) (*domain.ConsolidationOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range s.ops {
		if op.TenantID == tenantID && op.IdempotencyKey == key {
			cp := *op
			return &cp, nil
		}
	}
	return nil, nil
}
func (s *consolidationStore) List(context.Context, string, int, int) ([]*domain.ConsolidationOperation, error) {
	return nil, nil
}
func (s *consolidationStore) Claim(_ context.Context, id, tenantID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok || op.TenantID != tenantID || op.Status != domain.ConsolidationPending {
		return false, nil
	}
	op.Status = domain.ConsolidationProcessing
	return true, nil
}
func (s *consolidationStore) Finish(_ context.Context, op *domain.ConsolidationOperation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *op
	s.ops[op.ID] = &cp
	return nil
}

type consolidationGuards struct {
	settings *domain.TenantCustodySettings
	pending  map[string]int
}

func (g *consolidationGuards) GetCustodySettings(context.Context, string, domain.Mode) (*domain.TenantCustodySettings, error) {
	if g.settings == nil {
		return nil, domain.ErrCustodySettingsNotFound
	}
	return g.settings, nil
}
func (g *consolidationGuards) CountPendingTransfers(_ context.Context, walletID string) (int, error) {
	return g.pending[walletID], nil
}

type recordingEnqueuer struct {
	tasks []queue.ConsolidateWalletsPayload
}

func (e *recordingEnqueuer) Enqueue(_ context.Context, taskType string, payload interface{}) error {
	if taskType != queue.TypeConsolidateWallets {
		return fmt.Errorf("unexpected task %s", taskType)
	}
	e.tasks = append(e.tasks, payload.(queue.ConsolidateWalletsPayload))
	return nil
}

// ── fixture ──────────────────────────────────────────────────────────────────

type consolidationFixture struct {
	c        *Consolidator
	ledger   *testnetLedger
	wallets  *closureWallets
	store    *consolidationStore
	guards   *consolidationGuards
	audit    *closureAudit
	enqueuer *recordingEnqueuer
	dest     *domain.Wallet
	usdcKey  string
}

func newConsolidationFixture(t *testing.T) *consolidationFixture {
	t.Helper()
	f := &consolidationFixture{
		ledger:   newTestnetLedger(),
		wallets:  &closureWallets{byID: map[string]*domain.Wallet{}},
		store:    &consolidationStore{ops: map[string]*domain.ConsolidationOperation{}},
		guards:   &consolidationGuards{pending: map[string]int{}},
		audit:    &closureAudit{},
		enqueuer: &recordingEnqueuer{},
		usdcKey:  "USDC:" + keypair.MustRandom().Address(),
	}
	f.c = NewConsolidator(f.wallets, f.store, f.guards, f.ledger, stellar.NewEnvSigner(sweepMasterKey, "testnet"), f.audit, f.enqueuer)
	f.dest = f.wallet(t, "dest", consolidationTenant)
	f.ledger.fund(f.dest.PublicKey, "5", [3]string{f.usdcKey, "0", "1000000"})
	return f
}

// wallet creates a custodial wallet with a real key, stored as CreateWallet
// stores it.
func (f *consolidationFixture) wallet(t *testing.T, id, tenantID string) *domain.Wallet {
	t.Helper()
	kp := keypair.MustRandom()
	envelope, err := crypto.Encrypt([]byte(kp.Seed()), sweepMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	w := &domain.Wallet{ID: id, TenantID: &tenantID, PublicKey: kp.Address(),
		CustodyType: domain.CustodyCustodial, EncryptedSecret: hex.EncodeToString(envelope)}
	f.wallets.byID[id] = w
	return w
}

// sources creates n funded source wallets holding XLM and USDC.
func (f *consolidationFixture) sources(t *testing.T, n int, xlm, usdc string) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("src-%03d", i)
		w := f.wallet(t, ids[i], consolidationTenant)
		f.ledger.fund(w.PublicKey, xlm, [3]string{f.usdcKey, usdc, "1000000"})
	}
	return ids
}

func consolidationCtx() context.Context {
	ctx := tenant.WithID(context.Background(), consolidationTenant)
	return tenant.WithUser(ctx, "operator-9", "owner")
}

// runQueued executes every queued consolidation the way the worker does.
func (f *consolidationFixture) runQueued(t *testing.T) {
	t.Helper()
	for _, task := range f.enqueuer.tasks {
		body, _ := json.Marshal(task)
		if err := f.c.HandleConsolidate(context.Background(), asynq.NewTask(queue.TypeConsolidateWallets, body)); err != nil {
			t.Fatalf("execute: %v", err)
		}
	}
}

func dec7(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// ── tests ────────────────────────────────────────────────────────────────────

func TestConsolidation_EndToEnd_120WalletsInBoundedBatches(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 120, "10", "25")
	req := ConsolidationRequest{SourceWalletIDs: ids, DestinationWalletID: f.dest.ID, IdempotencyKey: "consolidate-120", BatchSize: 20}

	// Preview: 6 batches of 20 wallets, 2 payments each, priced exactly.
	dry := req
	dry.DryRun = true
	preview, err := f.c.Preview(consolidationCtx(), dry)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Batches) != 6 || f.ledger.submitted != 0 || len(f.store.ops) != 0 || len(f.enqueuer.tasks) != 0 {
		t.Fatalf("preview must plan 6 batches without side effects: %d batches", len(preview.Batches))
	}
	for _, b := range preview.Batches {
		if len(b.WalletIDs) != 20 || b.Operations != 40 || b.FeeStroops != 4000 {
			t.Fatalf("unexpected batch %+v", b)
		}
	}
	if preview.TotalFeeStroops != 24000 || preview.TotalFee != "0.0024000" || preview.ReserveRecovered != "0.0000000" {
		t.Fatalf("unexpected totals: %+v", preview)
	}
	wantUSDC := domain.ConsolidationAssetChange{AssetCode: "USDC", Issuer: f.usdcKey[5:], Before: "0.0000000", After: "3000.0000000"}
	// 120 x (10 - 1.5 reserve) less the fees.
	wantXLM := domain.ConsolidationAssetChange{AssetCode: "XLM", Before: "5.0000000", After: "1024.9976000"}
	if len(preview.ResultingBalances) != 2 || preview.ResultingBalances[0] != wantUSDC || preview.ResultingBalances[1] != wantXLM {
		t.Fatalf("resulting balances = %+v", preview.ResultingBalances)
	}

	// Execute.
	op, replayed, err := f.c.Submit(consolidationCtx(), req)
	if err != nil || replayed || op.Status != domain.ConsolidationPending || len(f.enqueuer.tasks) != 1 {
		t.Fatalf("submit: op=%+v replayed=%v err=%v", op, replayed, err)
	}
	f.runQueued(t)

	done, _ := f.c.Get(consolidationCtx(), op.ID)
	if done.Status != domain.ConsolidationCompleted || len(done.TransactionHashes) != 6 || len(done.Batches) != 6 {
		t.Fatalf("unexpected result: %+v", done)
	}
	if !done.TotalFee.Equal(dec7("0.0024")) {
		t.Fatalf("total fee = %s", done.TotalFee)
	}
	if got := f.ledger.credit(f.dest.PublicKey, f.usdcKey); !got.Equal(dec7("3000")) {
		t.Fatalf("destination USDC = %s, want 3000", got)
	}
	if got := f.ledger.native(f.dest.PublicKey); !got.Equal(dec7("1024.9976")) {
		t.Fatalf("destination XLM = %s, preview promised 1024.9976", got)
	}
	for _, id := range ids {
		w := f.wallets.byID[id]
		if !f.ledger.credit(w.PublicKey, f.usdcKey).IsZero() || !f.ledger.native(w.PublicKey).Equal(dec7("1.5")) {
			t.Fatalf("%s left with %s XLM / %s USDC, want exactly its 1.5 XLM reserve", id,
				f.ledger.native(w.PublicKey), f.ledger.credit(w.PublicKey, f.usdcKey))
		}
	}
	actions := []string{}
	for _, e := range f.audit.events {
		actions = append(actions, e.Action)
	}
	if len(actions) != 2 || actions[0] != AuditActionConsolidationRequested || actions[1] != AuditActionConsolidationFinished {
		t.Fatalf("audit trail = %v", actions)
	}
}

func TestConsolidation_MergeRecoversReserves(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 25, "10", "5")
	req := ConsolidationRequest{SourceWalletIDs: ids, DestinationWalletID: f.dest.ID, IdempotencyKey: "merge-25", MergeSources: true, BatchSize: 10}

	dry := req
	dry.DryRun = true
	preview, err := f.c.Preview(consolidationCtx(), dry)
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{}
	for _, b := range preview.Batches {
		sizes = append(sizes, len(b.WalletIDs))
	}
	// Each wallet: USDC payment, trustline removal, merge = 3 operations.
	if fmt.Sprint(sizes) != "[10 10 5]" || preview.TotalFeeStroops != 7500 {
		t.Fatalf("batches %v, fee %d", sizes, preview.TotalFeeStroops)
	}
	if preview.ReserveRecovered != "37.5000000" || preview.NetXLM != "37.4992500" {
		t.Fatalf("reserve recovered %s net %s", preview.ReserveRecovered, preview.NetXLM)
	}

	if _, _, err := f.c.Submit(consolidationCtx(), req); err != nil {
		t.Fatal(err)
	}
	f.runQueued(t)

	for _, id := range ids {
		if _, ok := f.ledger.accounts[f.wallets.byID[id].PublicKey]; ok {
			t.Fatalf("%s should have been merged away", id)
		}
	}
	if got := f.ledger.native(f.dest.PublicKey); !got.Equal(dec7("254.99925")) {
		t.Fatalf("destination XLM = %s, want 5 + 250 - 0.00075", got)
	}
	if got := f.ledger.credit(f.dest.PublicKey, f.usdcKey); !got.Equal(dec7("125")) {
		t.Fatalf("destination USDC = %s", got)
	}
	op := f.store.ops["cons-1"]
	if !op.ReserveRecovered.Equal(dec7("37.5")) || op.Status != domain.ConsolidationCompleted {
		t.Fatalf("unexpected operation: %+v", op)
	}
}

func TestConsolidation_IdempotencyKeyPreventsDuplicateSweeps(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 12, "10", "25")
	req := ConsolidationRequest{SourceWalletIDs: ids, DestinationWalletID: f.dest.ID, IdempotencyKey: "same-key"}

	first, _, err := f.c.Submit(consolidationCtx(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Same key, same sources in another order: a replay.
	reordered := req
	reordered.SourceWalletIDs = append([]string{ids[11]}, ids[:11]...)
	again, replayed, err := f.c.Submit(consolidationCtx(), reordered)
	if err != nil || !replayed || again.ID != first.ID || len(f.enqueuer.tasks) != 1 {
		t.Fatalf("retry must replay the original: replayed=%v err=%v tasks=%d", replayed, err, len(f.enqueuer.tasks))
	}
	// Same key, different body: refused.
	different := req
	different.SourceWalletIDs = ids[:5]
	if _, _, err := f.c.Submit(consolidationCtx(), different); !errors.Is(err, domain.ErrConsolidationKeyConflict) {
		t.Fatalf("want ErrConsolidationKeyConflict, got %v", err)
	}

	// A redelivered task runs the operation once.
	f.runQueued(t)
	f.runQueued(t)
	if f.ledger.submitted != 1 {
		t.Fatalf("want one submission, got %d", f.ledger.submitted)
	}
}

func TestConsolidation_TenantBoundary(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 3, "10", "25")
	foreign := f.wallet(t, "foreign", "other-tenant")
	f.ledger.fund(foreign.PublicKey, "100")

	for name, req := range map[string]ConsolidationRequest{
		"foreign source":      {SourceWalletIDs: append(ids, foreign.ID), DestinationWalletID: f.dest.ID, IdempotencyKey: "k"},
		"foreign destination": {SourceWalletIDs: ids, DestinationWalletID: foreign.ID, IdempotencyKey: "k"},
		"unknown source":      {SourceWalletIDs: []string{"nope"}, DestinationWalletID: f.dest.ID, IdempotencyKey: "k"},
	} {
		if _, _, err := f.c.Submit(consolidationCtx(), req); !errors.Is(err, domain.ErrWalletNotFound) {
			t.Errorf("%s: want ErrWalletNotFound, got %v", name, err)
		}
	}
	if f.ledger.submitted != 0 || len(f.store.ops) != 0 {
		t.Fatal("a rejected request must have no effect")
	}
}

func TestConsolidation_RequestValidation(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 2, "10", "25")
	base := ConsolidationRequest{SourceWalletIDs: ids, DestinationWalletID: f.dest.ID, IdempotencyKey: "k"}
	cases := map[string]func(r *ConsolidationRequest){
		"missing key":            func(r *ConsolidationRequest) { r.IdempotencyKey = "" },
		"batch too small":        func(r *ConsolidationRequest) { r.BatchSize = 9 },
		"batch too large":        func(r *ConsolidationRequest) { r.BatchSize = 21 },
		"no sources":             func(r *ConsolidationRequest) { r.SourceWalletIDs = nil },
		"duplicate source":       func(r *ConsolidationRequest) { r.SourceWalletIDs = []string{ids[0], ids[0]} },
		"destination as source":  func(r *ConsolidationRequest) { r.SourceWalletIDs = []string{ids[0], f.dest.ID} },
		"too many sources (501)": func(r *ConsolidationRequest) { r.SourceWalletIDs = make([]string, 501) },
	}
	for name, mutate := range cases {
		req := base
		mutate(&req)
		if _, _, err := f.c.Submit(consolidationCtx(), req); !errors.Is(err, domain.ErrConsolidationInvalid) {
			t.Errorf("%s: want ErrConsolidationInvalid, got %v", name, err)
		}
	}
	if _, _, err := f.c.Submit(tenant.WithID(context.Background(), consolidationTenant), base); !errors.Is(err, domain.ErrClosureOperatorRequired) {
		t.Errorf("unauthenticated: want ErrClosureOperatorRequired, got %v", err)
	}
	// A dry run needs no idempotency key.
	dry := base
	dry.IdempotencyKey, dry.DryRun = "", true
	if _, err := f.c.Preview(consolidationCtx(), dry); err != nil {
		t.Errorf("dry run without key: %v", err)
	}
}

func TestConsolidation_SkipsUnsafeWallets(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 4, "10", "25")
	f.guards.pending[ids[0]] = 1 // transfer in flight
	empty := f.wallet(t, "empty", consolidationTenant)
	f.ledger.fund(empty.PublicKey, "1") // nothing above the reserve
	unfunded := f.wallet(t, "unfunded", consolidationTenant)
	f.guards.settings = &domain.TenantCustodySettings{PrimarySettlementWalletID: &ids[1]}

	req := ConsolidationRequest{SourceWalletIDs: append(ids, empty.ID, unfunded.ID), DestinationWalletID: f.dest.ID, MergeSources: true, DryRun: true}
	preview, err := f.c.Preview(consolidationCtx(), req)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, w := range preview.Wallets {
		reasons[w.WalletID] = w.SkipReason
	}
	want := map[string]string{
		ids[0]: ConsolidationSkipPending, ids[1]: ConsolidationSkipProtected, ids[2]: "", ids[3]: "",
		empty.ID: "", unfunded.ID: ConsolidationSkipUnfunded,
	}
	for id, reason := range want {
		if reasons[id] != reason {
			t.Errorf("%s: skip %q, want %q", id, reasons[id], reason)
		}
	}

	// Without merging, the empty wallet has nothing to give.
	req.MergeSources = false
	preview, _ = f.c.Preview(consolidationCtx(), req)
	for _, w := range preview.Wallets {
		if w.WalletID == empty.ID && w.SkipReason != ConsolidationSkipEmpty {
			t.Errorf("empty wallet skip = %q", w.SkipReason)
		}
		if w.WalletID == ids[1] && w.SkipReason != "" {
			t.Errorf("the settlement wallet may give balances when not merging: %q", w.SkipReason)
		}
	}
}

func TestConsolidation_DestinationChecks(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 2, "10", "25")
	delete(f.ledger.accounts, f.dest.PublicKey)
	req := ConsolidationRequest{SourceWalletIDs: ids, DestinationWalletID: f.dest.ID, DryRun: true}
	if _, err := f.c.Preview(consolidationCtx(), req); !errors.Is(err, domain.ErrConsolidationDestinationUnfunded) {
		t.Fatalf("want ErrConsolidationDestinationUnfunded, got %v", err)
	}

	// A destination without the USDC trustline cannot take a merge.
	f.ledger.fund(f.dest.PublicKey, "5")
	req.MergeSources = true
	preview, err := f.c.Preview(consolidationCtx(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Batches) != 0 || preview.Wallets[0].SkipReason != ConsolidationSkipNoTrustline {
		t.Fatalf("merge into a destination without trustline: %+v", preview.Wallets)
	}
	req.DryRun, req.IdempotencyKey = false, "k"
	if _, _, err := f.c.Submit(consolidationCtx(), req); !errors.Is(err, domain.ErrConsolidationNothingToMove) {
		t.Fatalf("want ErrConsolidationNothingToMove, got %v", err)
	}
}

func TestConsolidation_FailedBatchIsRecordedAndOthersRun(t *testing.T) {
	f := newConsolidationFixture(t)
	ids := f.sources(t, 30, "10", "25")
	f.ledger.failOn[2] = true
	op, _, err := f.c.Submit(consolidationCtx(), ConsolidationRequest{
		SourceWalletIDs: ids, DestinationWalletID: f.dest.ID, IdempotencyKey: "partial", BatchSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runQueued(t)

	done, _ := f.c.Get(consolidationCtx(), op.ID)
	if done.Status != domain.ConsolidationPartiallyCompleted || len(done.TransactionHashes) != 2 || done.ErrorMessage == nil {
		t.Fatalf("unexpected result: %+v", done)
	}
	statuses := []string{}
	for _, b := range done.Batches {
		statuses = append(statuses, b.Status)
	}
	if fmt.Sprint(statuses) != "[completed failed completed]" {
		t.Fatalf("batch statuses = %v", statuses)
	}
	if got := f.ledger.credit(f.dest.PublicKey, f.usdcKey); !got.Equal(dec7("500")) {
		t.Fatalf("destination USDC = %s, want 2 of 3 batches (500)", got)
	}
}
