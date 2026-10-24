package wallet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/strkey"
	"github.com/stellar/go/txnbuild"
)

const (
	// maxSweepSignatures is Stellar's limit of 20 signatures per transaction.
	// Every swept wallet signs for its own payments, so it bounds how many
	// wallets one sweep transaction can aggregate.
	maxSweepSignatures = 20
	// maxSweepOperations is Stellar's limit of 100 operations per transaction.
	maxSweepOperations = 100

	defaultSweepPageSize    = 200
	defaultSweepConcurrency = 8

	nativeAssetCode = "XLM"
	stellarDecimals = 7
)

// Reasons a wallet or asset was left unswept, reported per run.
const (
	SweepSkipProtected      = "protected_account"
	SweepSkipPending        = "pending_transfers"
	SweepSkipUnfunded       = "unfunded"
	SweepSkipLoadFailed     = "load_failed"
	SweepSkipBelowThreshold = "below_threshold"
	SweepSkipNoTrustline    = "master_no_trustline"
	SweepSkipFeeUnpayable   = "fee_unpayable"
	SweepSkipMasterUnfunded = "master_unfunded"
)

// defaultSweepBaseReserve is Stellar's base reserve unit (0.5 XLM).
var defaultSweepBaseReserve = decimal.RequireFromString("0.5")

// SweepThreshold sweeps an asset once a wallet holds more than Threshold of
// it. For XLM the amount compared is the excess above the minimum reserve.
type SweepThreshold struct {
	AssetCode string
	// Issuer pins the asset; empty matches any issuer. A payment can only
	// land on a trustline the master address already holds, so an
	// issuer-less threshold never sweeps an asset the tenant does not accept.
	Issuer    string
	Threshold decimal.Decimal
}

// ParseSweepThresholds parses "XLM=25,USDC=100,EURC:G...=50": a comma
// separated list of ASSET[:ISSUER]=AMOUNT entries.
func ParseSweepThresholds(raw string) ([]SweepThreshold, error) {
	var out []SweepThreshold
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		asset, amount, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("sweep threshold %q: want ASSET[:ISSUER]=AMOUNT", entry)
		}
		code, issuer, _ := strings.Cut(strings.TrimSpace(asset), ":")
		code = strings.ToUpper(strings.TrimSpace(code))
		issuer = strings.TrimSpace(issuer)
		if code == "" {
			return nil, fmt.Errorf("sweep threshold %q: missing asset code", entry)
		}
		if code == nativeAssetCode && issuer != "" {
			return nil, fmt.Errorf("sweep threshold %q: XLM has no issuer", entry)
		}
		if issuer != "" && !strkey.IsValidEd25519PublicKey(issuer) {
			return nil, fmt.Errorf("sweep threshold %q: invalid issuer", entry)
		}
		threshold, err := decimal.NewFromString(strings.TrimSpace(amount))
		if err != nil || threshold.IsNegative() {
			return nil, fmt.Errorf("sweep threshold %q: amount must be a non-negative number", entry)
		}
		out = append(out, SweepThreshold{AssetCode: code, Issuer: issuer, Threshold: threshold})
	}
	return out, nil
}

// SweepConfig tunes the sweep engine. Zero values fall back to defaults.
type SweepConfig struct {
	Thresholds []SweepThreshold
	// BaseReserve is Stellar's base reserve unit; 0.5 XLM on every network.
	BaseReserve decimal.Decimal
	// Concurrency bounds in-flight Horizon account loads.
	Concurrency int
	// PageSize is how many wallets are loaded and batched at a time.
	PageSize int
	// BaseFee is the per-operation fee in stroops.
	BaseFee int64
}

// SweepEventDispatcher emits the wallet.swept webhook. webhook.Service
// satisfies it.
type SweepEventDispatcher interface {
	Dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) error
}

// SweepReport summarises one sweep run across every opted-in tenant.
type SweepReport struct {
	Targets            int            `json:"targets"`
	WalletsScanned     int            `json:"wallets_scanned"`
	WalletsSwept       int            `json:"wallets_swept"`
	Transactions       int            `json:"transactions"`
	FailedTransactions int            `json:"failed_transactions"`
	Skipped            map[string]int `json:"skipped"`
}

func (r *SweepReport) skip(reason string, n int) {
	if r.Skipped == nil {
		r.Skipped = map[string]int{}
	}
	r.Skipped[reason] += n
}

// SweepEngine aggregates customer deposits held in custodial wallets into each
// tenant's master address, leaving every wallet its minimum reserve (#94).
//
// A run scans the custodial wallets of every tenant that has opted in, works
// out per wallet what can move without touching reserves or open liabilities,
// and packs the resulting payments from many wallets into as few transactions
// as Stellar allows: up to 20 wallets (the signature limit) and 100 payments
// per transaction, one fee for the whole batch.
type SweepEngine struct {
	repo           SweepRepository
	stellar        stellar.Client
	clientResolver stellar.ClientResolver
	signer         stellar.Signer
	signerResolver stellar.SignerResolver
	events         SweepEventDispatcher
	cfg            SweepConfig
	now            func() time.Time
}

// NewSweepEngine constructs the sweep engine.
func NewSweepEngine(repo SweepRepository, client stellar.Client, signer stellar.Signer, events SweepEventDispatcher, cfg SweepConfig) *SweepEngine {
	if !cfg.BaseReserve.IsPositive() {
		cfg.BaseReserve = defaultSweepBaseReserve
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultSweepConcurrency
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = defaultSweepPageSize
	}
	if cfg.BaseFee <= 0 {
		cfg.BaseFee = txnbuild.MinBaseFee
	}
	return &SweepEngine{repo: repo, stellar: client, signer: signer, events: events, cfg: cfg, now: time.Now}
}

// WithClientResolver routes Horizon calls by each tenant's live/test mode.
func (e *SweepEngine) WithClientResolver(resolver stellar.ClientResolver) *SweepEngine {
	e.clientResolver = resolver
	return e
}

// WithSignerResolver routes signing by each tenant's live/test mode.
func (e *SweepEngine) WithSignerResolver(resolver stellar.SignerResolver) *SweepEngine {
	e.signerResolver = resolver
	return e
}

// MinimumBalance is the XLM an account must keep to exist on the ledger:
// (2 + subentries + entries it sponsors - entries sponsored for it) base
// reserves. With the 0.5 XLM base reserve that is 1 XLM for the account
// itself plus 0.5 XLM for every trustline, offer, data entry or signer.
func MinimumBalance(acct horizon.Account, baseReserve decimal.Decimal) decimal.Decimal {
	entries := 2 + int64(acct.SubentryCount) + int64(acct.NumSponsoring) - int64(acct.NumSponsored)
	if entries < 0 {
		entries = 0
	}
	return baseReserve.Mul(decimal.NewFromInt(entries))
}

// SweepAll runs one sweep over every opted-in tenant and mode. A failure for
// one tenant or batch is logged and counted, never fatal to the others.
func (e *SweepEngine) SweepAll(ctx context.Context) (SweepReport, error) {
	report := SweepReport{Skipped: map[string]int{}}
	if len(e.cfg.Thresholds) == 0 {
		return report, nil
	}
	targets, err := e.repo.ListSweepTargets(ctx)
	if err != nil {
		return report, err
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		report.Targets++
		tctx := tenant.WithMode(tenant.WithID(ctx, target.TenantID), target.Mode)
		if err := e.sweepTenant(tctx, target, &report); err != nil {
			log.Error().Err(err).Str("tenant_id", target.TenantID).Str("mode", string(target.Mode)).
				Msg("custodial sweep: tenant sweep failed")
		}
	}
	return report, nil
}

// sweepPayment is one asset to move out of one wallet.
type sweepPayment struct {
	assetCode     string
	issuer        string
	amount        decimal.Decimal
	balanceBefore decimal.Decimal
}

func (p sweepPayment) native() bool { return p.assetCode == nativeAssetCode && p.issuer == "" }

// walletSweep is the validated plan for one wallet.
type walletSweep struct {
	wallet  *domain.Wallet
	account horizon.Account
	// nativeExcess is the XLM above minimum balance and selling liabilities;
	// it is what the wallet could spend on the batch fee.
	nativeExcess decimal.Decimal
	payments     []sweepPayment
	feePaid      decimal.Decimal
	// merge closes the account after its payments: every trustline is
	// removed and the remaining XLM, reserves included, is merged into the
	// destination. Only consolidation sets it.
	merge      bool
	trustlines []domain.ClosedTrustline
}

// opCount is how many operations the wallet contributes to a transaction.
func (w *walletSweep) opCount() int {
	if w.merge {
		return len(w.payments) + len(w.trustlines) + 1
	}
	return len(w.payments)
}

// operations builds the wallet's operations, each sourced from the wallet so
// it signs for them: payments first, then (when merging) trustline removals
// and the AccountMerge. Stellar applies them in order, so every trustline is
// empty before it is removed and gone before the merge.
func (w *walletSweep) operations(destination string) []txnbuild.Operation {
	ops := make([]txnbuild.Operation, 0, w.opCount())
	for _, pay := range w.payments {
		ops = append(ops, &txnbuild.Payment{
			Destination:   destination,
			Amount:        pay.amount.StringFixed(stellarDecimals),
			Asset:         paymentAsset(pay),
			SourceAccount: w.wallet.PublicKey,
		})
	}
	if !w.merge {
		return ops
	}
	for _, tl := range w.trustlines {
		line, err := txnbuild.CreditAsset{Code: tl.AssetCode, Issuer: tl.Issuer}.ToChangeTrustAsset()
		if err != nil {
			// The asset came from Horizon, so it is always well formed.
			continue
		}
		ops = append(ops, &txnbuild.ChangeTrust{Line: line, Limit: "0", SourceAccount: w.wallet.PublicKey})
	}
	return append(ops, &txnbuild.AccountMerge{Destination: destination, SourceAccount: w.wallet.PublicKey})
}

// trustHeadroom is how much more of an asset the master address can receive.
// A nil value means the trustline has no effective limit.
type trustHeadroom map[string]*decimal.Decimal

func assetKey(code, issuer string) string { return code + ":" + issuer }

func (e *SweepEngine) sweepTenant(ctx context.Context, target *domain.TenantCustodySettings, report *SweepReport) error {
	client := e.client(ctx)
	master, err := stellar.LoadAccountWithContext(ctx, client, target.MasterAddress)
	if err != nil {
		if stellar.IsNotFound(err) {
			report.skip(SweepSkipMasterUnfunded, 1)
			return nil
		}
		return fmt.Errorf("load master address: %w", err)
	}
	headroom := masterHeadroom(master)

	afterID := ""
	for {
		wallets, err := e.repo.ListCustodialWallets(ctx, target.TenantID, target.Mode, afterID, e.cfg.PageSize)
		if err != nil {
			return err
		}
		if len(wallets) == 0 {
			return nil
		}
		afterID = wallets[len(wallets)-1].ID
		report.WalletsScanned += len(wallets)

		plans := e.planPage(ctx, client, target, wallets, headroom, report)
		for _, batch := range batchWallets(plans, maxSweepSignatures) {
			e.submitBatch(ctx, client, target, batch, report)
		}
		if len(wallets) < e.cfg.PageSize {
			return nil
		}
	}
}

// planPage loads each wallet's account concurrently, then plans sweeps in
// wallet order so trustline headroom on the master address is consumed
// deterministically.
func (e *SweepEngine) planPage(ctx context.Context, client stellar.Client, target *domain.TenantCustodySettings, wallets []*domain.Wallet, headroom trustHeadroom, report *SweepReport) []*walletSweep {
	type loaded struct {
		account horizon.Account
		skip    string
	}
	results := make([]loaded, len(wallets))
	sem := make(chan struct{}, e.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, w := range wallets {
		if w.PublicKey == target.MasterAddress ||
			(target.PrimarySettlementWalletID != nil && *target.PrimarySettlementWalletID == w.ID) {
			results[i].skip = SweepSkipProtected
			continue
		}
		wg.Add(1)
		go func(i int, w *domain.Wallet) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Sweeping a wallet with a transfer in flight could underfund it.
			pending, err := e.repo.CountPendingTransfers(ctx, w.ID)
			if err != nil {
				results[i].skip = SweepSkipLoadFailed
				return
			}
			if pending > 0 {
				results[i].skip = SweepSkipPending
				return
			}
			acct, err := stellar.LoadAccountWithContext(ctx, client, w.PublicKey)
			switch {
			case stellar.IsNotFound(err):
				results[i].skip = SweepSkipUnfunded
			case err != nil:
				results[i].skip = SweepSkipLoadFailed
			default:
				results[i].account = acct
			}
		}(i, w)
	}
	wg.Wait()

	var plans []*walletSweep
	for i, w := range wallets {
		if results[i].skip != "" {
			report.skip(results[i].skip, 1)
			continue
		}
		plan, noTrust := planWalletSweep(w, results[i].account, e.cfg, headroom)
		report.skip(SweepSkipNoTrustline, noTrust)
		if len(plan.payments) == 0 {
			report.skip(SweepSkipBelowThreshold, 1)
			continue
		}
		plans = append(plans, plan)
	}
	return plans
}

// planWalletSweep decides what leaves one wallet. XLM above the minimum
// balance and native selling liabilities is swept once it exceeds the XLM
// threshold; any other asset is swept, less its selling liabilities, once
// that exceeds its threshold and the master address can receive it. It
// returns how many asset payments were dropped for want of a master trustline.
func planWalletSweep(w *domain.Wallet, acct horizon.Account, cfg SweepConfig, headroom trustHeadroom) (*walletSweep, int) {
	plan := &walletSweep{wallet: w, account: acct, nativeExcess: decimal.Zero, feePaid: decimal.Zero}
	minBalance := MinimumBalance(acct, cfg.BaseReserve)
	noTrust := 0

	for _, b := range acct.Balances {
		if b.Type == "liquidity_pool_shares" || b.LiquidityPoolId != "" {
			continue
		}
		balance, err := decimal.NewFromString(b.Balance)
		if err != nil {
			continue
		}
		selling := parseAmount(b.SellingLiabilities)

		if b.Type == "native" || (b.Code == "" && b.Issuer == "") {
			excess := balance.Sub(minBalance).Sub(selling).Truncate(stellarDecimals)
			if excess.IsPositive() {
				plan.nativeExcess = excess
			}
			if th, ok := thresholdFor(cfg.Thresholds, nativeAssetCode, ""); ok && excess.GreaterThan(th) {
				plan.payments = append(plan.payments, sweepPayment{
					assetCode: nativeAssetCode, amount: excess, balanceBefore: balance,
				})
			}
			continue
		}

		// A frozen trustline cannot send; leave it for an operator.
		if b.IsAuthorized != nil && !*b.IsAuthorized {
			continue
		}
		available := balance.Sub(selling).Truncate(stellarDecimals)
		th, ok := thresholdFor(cfg.Thresholds, b.Code, b.Issuer)
		if !ok || !available.GreaterThan(th) {
			continue
		}
		room, trusted := headroom[assetKey(b.Code, b.Issuer)]
		if !trusted || (room != nil && available.GreaterThan(*room)) {
			noTrust++
			continue
		}
		if room != nil {
			left := room.Sub(available)
			headroom[assetKey(b.Code, b.Issuer)] = &left
		}
		plan.payments = append(plan.payments, sweepPayment{
			assetCode: b.Code, issuer: b.Issuer, amount: available, balanceBefore: balance,
		})
	}

	// Stable order: credit assets by code, XLM last so it reads like a statement.
	sort.SliceStable(plan.payments, func(i, j int) bool {
		if plan.payments[i].native() != plan.payments[j].native() {
			return !plan.payments[i].native()
		}
		return assetKey(plan.payments[i].assetCode, plan.payments[i].issuer) < assetKey(plan.payments[j].assetCode, plan.payments[j].issuer)
	})
	if len(plan.payments) > maxSweepOperations {
		plan.payments = plan.payments[:maxSweepOperations]
	}
	return plan, noTrust
}

// masterHeadroom lists the assets the master address can receive and how much
// more of each its trustline limit allows.
func masterHeadroom(master horizon.Account) trustHeadroom {
	out := trustHeadroom{}
	for _, b := range master.Balances {
		if b.Type == "native" || b.Type == "liquidity_pool_shares" || b.Code == "" {
			continue
		}
		if b.IsAuthorized != nil && !*b.IsAuthorized {
			continue
		}
		limit, err := decimal.NewFromString(b.Limit)
		if err != nil || b.Limit == "" {
			out[assetKey(b.Code, b.Issuer)] = nil
			continue
		}
		room := limit.Sub(parseAmount(b.Balance)).Sub(parseAmount(b.BuyingLiabilities))
		out[assetKey(b.Code, b.Issuer)] = &room
	}
	return out
}

// batchWallets packs wallet plans greedily, in order, into transactions of at
// most maxWallets wallets (one signature each) and maxSweepOperations
// operations.
func batchWallets(plans []*walletSweep, maxWallets int) [][]*walletSweep {
	if maxWallets <= 0 || maxWallets > maxSweepSignatures {
		maxWallets = maxSweepSignatures
	}
	var batches [][]*walletSweep
	var current []*walletSweep
	ops := 0
	for _, p := range plans {
		if len(current) > 0 && (len(current) == maxWallets || ops+p.opCount() > maxSweepOperations) {
			batches = append(batches, current)
			current, ops = nil, 0
		}
		current = append(current, p)
		ops += p.opCount()
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// assignFee makes the wallet with the most spare XLM the transaction source
// and fee payer. When that wallet is also sweeping XLM, the fee comes out of
// its sweep so its reserve is never touched. It returns the payer, or nil when
// no wallet in the batch can cover the fee from spare XLM.
func assignFee(batch []*walletSweep, baseFee int64) (*walletSweep, int64) {
	var payer *walletSweep
	for _, p := range batch {
		if payer == nil || p.nativeExcess.GreaterThan(payer.nativeExcess) {
			payer = p
		}
	}
	ops := 0
	for _, p := range batch {
		ops += p.opCount()
	}
	fee := baseFee * int64(ops)
	if payer == nil || payer.nativeExcess.LessThan(stroopsToXLM(fee)) {
		return nil, fee
	}
	for i, pay := range payer.payments {
		if !pay.native() {
			continue
		}
		remaining := pay.amount.Sub(stroopsToXLM(fee))
		if remaining.IsPositive() {
			payer.payments[i].amount = remaining
		} else {
			// Too little XLM to sweep after the fee: keep it and pay the
			// smaller fee of one fewer operation from it instead.
			payer.payments = append(payer.payments[:i], payer.payments[i+1:]...)
			fee -= baseFee
		}
		break
	}
	payer.feePaid = stroopsToXLM(fee)
	return payer, fee
}

func (e *SweepEngine) submitBatch(ctx context.Context, client stellar.Client, target *domain.TenantCustodySettings, batch []*walletSweep, report *SweepReport) {
	payer, fee := assignFee(batch, e.cfg.BaseFee)
	if payer == nil {
		report.skip(SweepSkipFeeUnpayable, len(batch))
		return
	}

	var ops []txnbuild.Operation
	for _, p := range batch {
		ops = append(ops, p.operations(target.MasterAddress)...)
	}
	sweep := &domain.CustodialSweep{
		ID:                 uuid.New().String(),
		TenantID:           target.TenantID,
		Mode:               target.Mode,
		DestinationAddress: target.MasterAddress,
		FeeStroops:         fee,
		Items:              sweepItems(batch),
	}

	hash, err := e.buildSignSubmit(ctx, client, payer, batch, ops)
	if err != nil {
		msg := err.Error()
		sweep.Status = domain.SweepStatusFailed
		sweep.ErrorMessage = &msg
		report.FailedTransactions++
		if recErr := e.repo.RecordSweep(ctx, sweep); recErr != nil {
			log.Error().Err(recErr).Msg("custodial sweep: failed to record failed sweep")
		}
		log.Error().Err(err).Str("tenant_id", target.TenantID).Int("wallets", len(batch)).
			Msg("custodial sweep: batch transaction failed")
		return
	}

	sweep.Status = domain.SweepStatusCompleted
	sweep.TransactionHash = &hash
	report.Transactions++
	report.WalletsSwept += len(batch)
	// The funds have moved; a bookkeeping failure must not hide that.
	if err := e.repo.RecordSweep(ctx, sweep); err != nil {
		log.Error().Err(err).Str("tx_hash", hash).Msg("custodial sweep: submitted but not recorded")
	}
	for _, p := range batch {
		e.emitSwept(ctx, sweep, p)
	}
}

func (e *SweepEngine) buildSignSubmit(ctx context.Context, client stellar.Client, payer *walletSweep, batch []*walletSweep, ops []txnbuild.Operation) (string, error) {
	source := payer.account
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &source,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              e.cfg.BaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(60)},
	})
	if err != nil {
		return "", fmt.Errorf("build sweep transaction: %w", err)
	}
	signer := e.signerFor(ctx)
	if signer == nil {
		return "", errors.New("no signer configured")
	}
	// Each wallet authorises the payments it is the source of.
	for _, p := range batch {
		if tx, err = signWithStoredSecret(signer, tx, p.wallet.EncryptedSecret); err != nil {
			return "", fmt.Errorf("sign sweep for wallet %s: %w", p.wallet.ID, err)
		}
	}
	resp, err := stellar.SubmitTransactionWithContext(ctx, client, tx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", domain.ErrStellarSubmission, err)
	}
	return resp.Hash, nil
}

// emitSwept sends one wallet.swept event per wallet, itemising what moved.
func (e *SweepEngine) emitSwept(ctx context.Context, sweep *domain.CustodialSweep, p *walletSweep) {
	if e.events == nil {
		return
	}
	items := []domain.SweepItem{}
	for _, item := range sweep.Items {
		if item.WalletID == p.wallet.ID {
			items = append(items, item)
		}
	}
	payload := map[string]interface{}{
		"sweep_id":            sweep.ID,
		"wallet_id":           p.wallet.ID,
		"public_key":          p.wallet.PublicKey,
		"destination_address": sweep.DestinationAddress,
		"transaction_hash":    *sweep.TransactionHash,
		"mode":                sweep.Mode,
		"items":               items,
		"fee_paid":            p.feePaid.StringFixed(stellarDecimals),
		"swept_at":            e.now().UTC(),
	}
	if err := e.events.Dispatch(ctx, domain.EventWalletSwept, payload); err != nil {
		log.Error().Err(err).Str("wallet_id", p.wallet.ID).Msg("custodial sweep: failed to emit wallet.swept")
	}
}

func sweepItems(batch []*walletSweep) []domain.SweepItem {
	var items []domain.SweepItem
	for _, p := range batch {
		for _, pay := range p.payments {
			retained := pay.balanceBefore.Sub(pay.amount)
			if pay.native() {
				retained = retained.Sub(p.feePaid)
			}
			items = append(items, domain.SweepItem{
				WalletID:      p.wallet.ID,
				PublicKey:     p.wallet.PublicKey,
				AssetCode:     pay.assetCode,
				Issuer:        pay.issuer,
				Amount:        pay.amount.StringFixed(stellarDecimals),
				BalanceBefore: pay.balanceBefore.StringFixed(stellarDecimals),
				Retained:      retained.StringFixed(stellarDecimals),
			})
		}
	}
	return items
}

func paymentAsset(p sweepPayment) txnbuild.Asset {
	if p.native() {
		return txnbuild.NativeAsset{}
	}
	return txnbuild.CreditAsset{Code: p.assetCode, Issuer: p.issuer}
}

// thresholdFor prefers an issuer-pinned threshold over a code-only one.
func thresholdFor(thresholds []SweepThreshold, code, issuer string) (decimal.Decimal, bool) {
	var wildcard *decimal.Decimal
	for i := range thresholds {
		t := thresholds[i]
		if !strings.EqualFold(t.AssetCode, code) {
			continue
		}
		if t.Issuer == issuer && issuer != "" {
			return t.Threshold, true
		}
		if t.Issuer == "" && wildcard == nil {
			wildcard = &thresholds[i].Threshold
		}
	}
	if wildcard != nil {
		return *wildcard, true
	}
	return decimal.Zero, false
}

func stroopsToXLM(stroops int64) decimal.Decimal { return decimal.New(stroops, -stellarDecimals) }

func parseAmount(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func (e *SweepEngine) client(ctx context.Context) stellar.Client {
	if e.clientResolver != nil {
		if client := e.clientResolver.ClientForMode(ctx); client != nil {
			return client
		}
	}
	return e.stellar
}

func (e *SweepEngine) signerFor(ctx context.Context) stellar.Signer {
	if e.signerResolver != nil {
		if s := e.signerResolver.SignerForMode(ctx); s != nil {
			return s
		}
	}
	return e.signer
}
