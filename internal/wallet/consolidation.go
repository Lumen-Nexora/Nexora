package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/queue"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
)

const (
	// MinConsolidationBatch and MaxConsolidationBatch bound how many source
	// wallets go into one Stellar transaction. The upper bound is Stellar's
	// 20-signature limit; a batch also closes early at 100 operations.
	MinConsolidationBatch     = 10
	MaxConsolidationBatch     = maxSweepSignatures
	DefaultConsolidationBatch = MaxConsolidationBatch
	// MaxConsolidationSources caps one request so a run stays well inside
	// the worker's claim window.
	MaxConsolidationSources = 500
	maxIdempotencyKeyLength = 255
)

// Audit actions written by consolidation.
const (
	AuditActionConsolidationRequested = "wallet.consolidation.requested"
	AuditActionConsolidationFinished  = "wallet.consolidation.finished"
)

// Reasons a source wallet is left out of a consolidation.
const (
	ConsolidationSkipPending     = "pending_transfers"
	ConsolidationSkipUnfunded    = "unfunded"
	ConsolidationSkipLoadFailed  = "load_failed"
	ConsolidationSkipEmpty       = "nothing_to_move"
	ConsolidationSkipProtected   = "protected_account"
	ConsolidationSkipNoTrustline = "destination_missing_trustline"
	ConsolidationSkipUnsupported = "unsupported_account"
	ConsolidationSkipFee         = "fee_unpayable"
)

// ConsolidationRequest asks to move the balances of SourceWalletIDs into
// DestinationWalletID, all of which must belong to the calling tenant.
type ConsolidationRequest struct {
	SourceWalletIDs     []string `json:"source_wallet_ids"`
	DestinationWalletID string   `json:"destination_wallet_id"`
	// IdempotencyKey is required unless DryRun is set. Retrying with the same
	// key and body returns the original operation; a different body is
	// refused.
	IdempotencyKey string `json:"idempotency_key"`
	// MergeSources closes every source account after moving its balances:
	// trustlines are removed and the account is merged into the destination,
	// recovering its reserves.
	MergeSources bool `json:"merge_sources"`
	// BatchSize is how many source wallets go into each transaction, 10-20.
	BatchSize int  `json:"batch_size"`
	DryRun    bool `json:"dry_run"`
}

// ConsolidationGuards supplies the safety checks consolidation shares with
// account closure. ClosureRepository satisfies it.
type ConsolidationGuards interface {
	GetCustodySettings(ctx context.Context, tenantID string, mode domain.Mode) (*domain.TenantCustodySettings, error)
	CountPendingTransfers(ctx context.Context, walletID string) (int, error)
}

// TaskEnqueuer queues background work. queue.Client satisfies it.
type TaskEnqueuer interface {
	Enqueue(ctx context.Context, taskType string, payload interface{}) error
}

// Consolidator aggregates balances from many of a tenant's custodial wallets
// into one of them (#95). A request is validated and priced synchronously,
// then executed by the worker in sequential batches of 10-20 wallets per
// Stellar transaction, so 100+ wallets never become one oversized
// transaction or one long HTTP request.
type Consolidator struct {
	wallets        Repository
	ops            ConsolidationRepository
	guards         ConsolidationGuards
	stellar        stellar.Client
	clientResolver stellar.ClientResolver
	signer         stellar.Signer
	signerResolver stellar.SignerResolver
	audit          AuditRecorder
	enqueuer       TaskEnqueuer
	baseReserve    decimal.Decimal
	baseFee        int64
	concurrency    int
	now            func() time.Time
}

// NewConsolidator constructs the consolidation engine. enqueuer may be nil
// in the worker, which only executes.
func NewConsolidator(wallets Repository, ops ConsolidationRepository, guards ConsolidationGuards, client stellar.Client, signer stellar.Signer, audit AuditRecorder, enqueuer TaskEnqueuer) *Consolidator {
	return &Consolidator{
		wallets: wallets, ops: ops, guards: guards, stellar: client, signer: signer,
		audit: audit, enqueuer: enqueuer,
		baseReserve: defaultSweepBaseReserve, baseFee: txnbuild.MinBaseFee,
		concurrency: defaultSweepConcurrency, now: time.Now,
	}
}

// WithClientResolver routes Horizon calls by the request's live/test mode.
func (c *Consolidator) WithClientResolver(resolver stellar.ClientResolver) *Consolidator {
	c.clientResolver = resolver
	return c
}

// WithSignerResolver routes signing by the request's live/test mode.
func (c *Consolidator) WithSignerResolver(resolver stellar.SignerResolver) *Consolidator {
	c.signerResolver = resolver
	return c
}

// Preview prices a consolidation against current ledger state without
// submitting or recording anything.
func (c *Consolidator) Preview(ctx context.Context, req ConsolidationRequest) (*domain.ConsolidationPreview, error) {
	sources, dest, batchSize, err := c.validate(ctx, req, false)
	if err != nil {
		return nil, err
	}
	plan, err := c.plan(ctx, sources, dest, req.MergeSources, batchSize)
	if err != nil {
		return nil, err
	}
	return plan.preview(dest, req.MergeSources, batchSize, c.baseReserve), nil
}

// Submit validates and prices a consolidation, records it under the caller's
// idempotency key and queues it for execution. replayed is true when the key
// was already used for this same request, in which case nothing new happens.
func (c *Consolidator) Submit(ctx context.Context, req ConsolidationRequest) (op *domain.ConsolidationOperation, replayed bool, err error) {
	sources, dest, batchSize, err := c.validate(ctx, req, true)
	if err != nil {
		return nil, false, err
	}
	tenantID := tenant.IDFromContext(ctx)
	fingerprint := consolidationFingerprint(req, batchSize)

	if existing, err := c.ops.GetByIdempotencyKey(ctx, tenantID, req.IdempotencyKey); err != nil {
		return nil, false, err
	} else if existing != nil {
		return replay(existing, fingerprint)
	}

	plan, err := c.plan(ctx, sources, dest, req.MergeSources, batchSize)
	if err != nil {
		return nil, false, err
	}
	if len(plan.batches) == 0 {
		return nil, false, domain.ErrConsolidationNothingToMove
	}
	preview := plan.preview(dest, req.MergeSources, batchSize, c.baseReserve)

	actorType, actorID := closureOperator(ctx)
	op = &domain.ConsolidationOperation{
		TenantID:          tenantID,
		Mode:              tenant.ModeOrDefault(ctx, domain.ModeLive),
		SourceWalletIDs:   walletIDs(sources),
		DestinationWallet: dest.ID,
		IdempotencyKey:    req.IdempotencyKey,
		MergeSources:      req.MergeSources,
		BatchSize:         batchSize,
		TotalFee:          stroopsToXLM(preview.TotalFeeStroops),
		ReserveRecovered:  decimal.RequireFromString(preview.ReserveRecovered),
		ActorType:         actorType,
		ActorID:           actorID,
		Fingerprint:       fingerprint,
	}
	if err := c.ops.Create(ctx, op); err != nil {
		// A concurrent request with the same key won the insert.
		if errors.Is(err, domain.ErrConsolidationKeyConflict) {
			if existing, getErr := c.ops.GetByIdempotencyKey(ctx, tenantID, req.IdempotencyKey); getErr == nil && existing != nil {
				return replay(existing, fingerprint)
			}
		}
		return nil, false, err
	}
	c.record(ctx, AuditActionConsolidationRequested, op)

	if c.enqueuer == nil {
		return op, false, nil
	}
	if err := c.enqueuer.Enqueue(ctx, queue.TypeConsolidateWallets, queue.ConsolidateWalletsPayload{
		OperationID: op.ID, TenantID: tenantID, Mode: string(op.Mode),
	}); err != nil {
		// Never leave the key pointing at an operation nobody will run.
		msg := "enqueue consolidation: " + err.Error()
		now := c.now().UTC()
		op.Status, op.ErrorMessage, op.CompletedAt = domain.ConsolidationFailed, &msg, &now
		if finishErr := c.ops.Finish(ctx, op); finishErr != nil {
			log.Error().Err(finishErr).Str("consolidation_id", op.ID).Msg("failed to mark unqueued consolidation failed")
		}
		return nil, false, fmt.Errorf("enqueue consolidation: %w", err)
	}
	return op, false, nil
}

// Execute runs an accepted consolidation. It re-plans against the ledger as
// it is now, then submits the batches one after another; a failed batch is
// recorded and the remaining batches still run. Running it twice is safe:
// only one worker can claim the operation, and a finished one is skipped.
func (c *Consolidator) Execute(ctx context.Context, id string) error {
	tenantID := tenant.IDFromContext(ctx)
	claimed, err := c.ops.Claim(ctx, id, tenantID)
	if err != nil || !claimed {
		return err
	}
	op, err := c.ops.Get(ctx, id, tenantID)
	if err != nil {
		return err
	}

	finish := func(status string, cause error) error {
		now := c.now().UTC()
		op.Status, op.CompletedAt = status, &now
		if cause != nil {
			msg := cause.Error()
			op.ErrorMessage = &msg
		}
		if err := c.ops.Finish(ctx, op); err != nil {
			return err
		}
		c.record(ctx, AuditActionConsolidationFinished, op)
		return nil
	}

	sources := make([]*domain.Wallet, 0, len(op.SourceWalletIDs))
	for _, wid := range op.SourceWalletIDs {
		w, err := c.wallets.GetByID(ctx, wid)
		if err != nil {
			return finish(domain.ConsolidationFailed, fmt.Errorf("load source wallet %s: %w", wid, err))
		}
		sources = append(sources, w)
	}
	dest, err := c.wallets.GetByID(ctx, op.DestinationWallet)
	if err != nil {
		return finish(domain.ConsolidationFailed, fmt.Errorf("load destination wallet: %w", err))
	}
	plan, err := c.plan(ctx, sources, dest, op.MergeSources, op.BatchSize)
	if err != nil {
		return finish(domain.ConsolidationFailed, err)
	}

	client := c.client(ctx)
	op.Batches = []domain.ConsolidationBatch{}
	op.TransactionHashes = []string{}
	totalFee, recovered := int64(0), decimal.Zero
	succeeded, failed := 0, 0
	for i, batch := range plan.batches {
		result := batchSummary(i, batch, plan.payers[i], plan.fees[i])
		hash, err := c.submit(ctx, client, dest.PublicKey, plan.payers[i], batch)
		if err != nil {
			failed++
			result.Status, result.Error = domain.ConsolidationFailed, err.Error()
			log.Error().Err(err).Str("consolidation_id", op.ID).Int("batch", i).Msg("consolidation batch failed")
		} else {
			succeeded++
			result.Status, result.TransactionHash = domain.ConsolidationCompleted, hash
			op.TransactionHashes = append(op.TransactionHashes, hash)
			totalFee += plan.fees[i]
			for _, p := range batch {
				if p.merge {
					recovered = recovered.Add(MinimumBalance(p.account, c.baseReserve))
				}
			}
		}
		op.Batches = append(op.Batches, result)
	}
	op.TotalFee, op.ReserveRecovered = stroopsToXLM(totalFee), recovered

	switch {
	case succeeded == 0 && failed == 0:
		return finish(domain.ConsolidationCompleted, nil) // already consolidated
	case failed == 0:
		return finish(domain.ConsolidationCompleted, nil)
	case succeeded == 0:
		return finish(domain.ConsolidationFailed, errors.New("every batch failed"))
	default:
		return finish(domain.ConsolidationPartiallyCompleted, fmt.Errorf("%d of %d batches failed", failed, failed+succeeded))
	}
}

// Get returns one of the calling tenant's consolidations.
func (c *Consolidator) Get(ctx context.Context, id string) (*domain.ConsolidationOperation, error) {
	return c.ops.Get(ctx, id, tenant.IDFromContext(ctx))
}

// List returns the calling tenant's consolidations, newest first.
func (c *Consolidator) List(ctx context.Context, limit, offset int) ([]*domain.ConsolidationOperation, error) {
	return c.ops.List(ctx, tenant.IDFromContext(ctx), limit, offset)
}

// HandleConsolidate is the worker handler for queue.TypeConsolidateWallets.
// A run's own batch failures are recorded on the operation, so only an
// infrastructure error (claiming or saving it) is returned for retry.
func (c *Consolidator) HandleConsolidate(ctx context.Context, task *asynq.Task) error {
	ctx = queue.ContextFromTask(ctx, task)
	var payload queue.ConsolidateWalletsPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("decode consolidation payload: %w", asynq.SkipRetry)
	}
	ctx = tenant.WithMode(tenant.WithID(ctx, payload.TenantID), domain.Mode(payload.Mode))
	return c.Execute(ctx, payload.OperationID)
}

// validate checks the request and resolves every wallet strictly inside the
// calling tenant.
func (c *Consolidator) validate(ctx context.Context, req ConsolidationRequest, requireKey bool) ([]*domain.Wallet, *domain.Wallet, int, error) {
	tenantID := tenant.IDFromContext(ctx)
	if _, actorID := closureOperator(ctx); tenantID == "" || actorID == "" {
		return nil, nil, 0, domain.ErrClosureOperatorRequired
	}
	invalid := func(msg string, args ...interface{}) error {
		return fmt.Errorf("%w: %s", domain.ErrConsolidationInvalid, fmt.Sprintf(msg, args...))
	}
	if requireKey {
		key := strings.TrimSpace(req.IdempotencyKey)
		if key == "" || len(key) > maxIdempotencyKeyLength || key != req.IdempotencyKey {
			return nil, nil, 0, invalid("idempotency_key is required (1-%d characters, no surrounding spaces)", maxIdempotencyKeyLength)
		}
	}
	batchSize := req.BatchSize
	if batchSize == 0 {
		batchSize = DefaultConsolidationBatch
	}
	if batchSize < MinConsolidationBatch || batchSize > MaxConsolidationBatch {
		return nil, nil, 0, invalid("batch_size must be between %d and %d", MinConsolidationBatch, MaxConsolidationBatch)
	}
	if len(req.SourceWalletIDs) == 0 || len(req.SourceWalletIDs) > MaxConsolidationSources {
		return nil, nil, 0, invalid("between 1 and %d source wallets are required", MaxConsolidationSources)
	}

	dest, err := c.tenantWallet(ctx, req.DestinationWalletID)
	if err != nil {
		return nil, nil, 0, err
	}
	seen := map[string]bool{}
	sources := make([]*domain.Wallet, 0, len(req.SourceWalletIDs))
	for _, id := range req.SourceWalletIDs {
		if seen[id] {
			return nil, nil, 0, invalid("source wallet %s is listed twice", id)
		}
		seen[id] = true
		if id == dest.ID {
			return nil, nil, 0, invalid("the destination wallet cannot also be a source")
		}
		w, err := c.tenantWallet(ctx, id)
		if err != nil {
			return nil, nil, 0, err
		}
		sources = append(sources, w)
	}
	return sources, dest, batchSize, nil
}

// tenantWallet loads a custodial wallet and proves it belongs to the caller.
// A wallet of another tenant is reported as not found, never as forbidden.
func (c *Consolidator) tenantWallet(ctx context.Context, id string) (*domain.Wallet, error) {
	w, err := c.wallets.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.TenantID == nil || *w.TenantID != tenant.IDFromContext(ctx) {
		return nil, domain.ErrWalletNotFound
	}
	if w.CustodyType != "" && w.CustodyType != domain.CustodyCustodial {
		return nil, fmt.Errorf("%w: wallet %s is not custodial", domain.ErrConsolidationInvalid, w.ID)
	}
	return w, nil
}

// consolidationPlan is the batched, fee-assigned plan for one run.
type consolidationPlan struct {
	batches  [][]*walletSweep
	payers   []*walletSweep
	fees     []int64
	wallets  []domain.ConsolidationWalletPlan
	destAcct horizon.Account
}

func (c *Consolidator) plan(ctx context.Context, sources []*domain.Wallet, dest *domain.Wallet, merge bool, batchSize int) (*consolidationPlan, error) {
	client := c.client(ctx)
	destAcct, err := stellar.LoadAccountWithContext(ctx, client, dest.PublicKey)
	if err != nil {
		if stellar.IsNotFound(err) {
			return nil, domain.ErrConsolidationDestinationUnfunded
		}
		return nil, fmt.Errorf("load destination account: %w", err)
	}
	headroom := masterHeadroom(destAcct)

	var protected map[string]bool
	if merge {
		protected = map[string]bool{}
		settings, err := c.guards.GetCustodySettings(ctx, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive))
		if err != nil && !errors.Is(err, domain.ErrCustodySettingsNotFound) {
			return nil, err
		}
		if settings != nil {
			if settings.PrimarySettlementWalletID != nil {
				protected[*settings.PrimarySettlementWalletID] = true
			}
			protected["pk:"+settings.MasterAddress] = true
		}
	}

	type loaded struct {
		account horizon.Account
		skip    string
	}
	results := make([]loaded, len(sources))
	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup
	for i, w := range sources {
		wg.Add(1)
		go func(i int, w *domain.Wallet) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pending, err := c.guards.CountPendingTransfers(ctx, w.ID)
			switch {
			case err != nil:
				results[i].skip = ConsolidationSkipLoadFailed
				return
			case pending > 0:
				results[i].skip = ConsolidationSkipPending
				return
			}
			acct, err := stellar.LoadAccountWithContext(ctx, client, w.PublicKey)
			switch {
			case stellar.IsNotFound(err):
				results[i].skip = ConsolidationSkipUnfunded
			case err != nil:
				results[i].skip = ConsolidationSkipLoadFailed
			default:
				results[i].account = acct
			}
		}(i, w)
	}
	wg.Wait()

	plan := &consolidationPlan{destAcct: destAcct}
	var movable []*walletSweep
	skipped := map[string]string{}
	for i, w := range sources {
		skip := results[i].skip
		var ws *walletSweep
		if skip == "" && merge && (protected[w.ID] || protected["pk:"+w.PublicKey]) {
			skip = ConsolidationSkipProtected
		}
		if skip == "" {
			if merge {
				ws, skip = planMergeConsolidation(w, results[i].account, headroom)
			} else {
				ws, skip = c.planMoveConsolidation(w, results[i].account, headroom)
			}
		}
		if skip != "" {
			skipped[w.ID] = skip
			continue
		}
		movable = append(movable, ws)
	}

	for _, batch := range batchWallets(movable, batchSize) {
		payer, fee := assignFee(batch, c.baseFee)
		if payer == nil {
			for _, p := range batch {
				skipped[p.wallet.ID] = ConsolidationSkipFee
			}
			continue
		}
		plan.batches = append(plan.batches, batch)
		plan.payers = append(plan.payers, payer)
		plan.fees = append(plan.fees, fee)
	}

	inPlan := map[string]*walletSweep{}
	for _, batch := range plan.batches {
		for _, p := range batch {
			inPlan[p.wallet.ID] = p
		}
	}
	for _, w := range sources {
		entry := domain.ConsolidationWalletPlan{WalletID: w.ID, PublicKey: w.PublicKey, SkipReason: skipped[w.ID]}
		if p := inPlan[w.ID]; p != nil {
			entry.Merged = p.merge
			entry.Moves = consolidationMoves(p, c.baseReserve)
		}
		plan.wallets = append(plan.wallets, entry)
	}
	return plan, nil
}

// planMoveConsolidation moves every non-native balance (less selling
// liabilities) the destination can receive, and all XLM above the minimum
// balance, leaving the source account open.
func (c *Consolidator) planMoveConsolidation(w *domain.Wallet, acct horizon.Account, headroom trustHeadroom) (*walletSweep, string) {
	everything := SweepConfig{BaseReserve: c.baseReserve, Thresholds: []SweepThreshold{{AssetCode: nativeAssetCode}}}
	for _, b := range acct.Balances {
		if b.Code != "" {
			everything.Thresholds = append(everything.Thresholds, SweepThreshold{AssetCode: b.Code, Issuer: b.Issuer})
		}
	}
	plan, noTrust := planWalletSweep(w, acct, everything, headroom)
	if len(plan.payments) == 0 {
		if noTrust > 0 {
			return nil, ConsolidationSkipNoTrustline
		}
		return nil, ConsolidationSkipEmpty
	}
	return plan, ""
}

// planMergeConsolidation empties and closes the source: every non-native
// balance is paid to the destination, every trustline removed, and the
// account merged so its reserves move too. The account must hold nothing a
// merge cannot carry, and the destination must be able to receive every
// asset, or the wallet is skipped as a whole.
func planMergeConsolidation(w *domain.Wallet, acct horizon.Account, headroom trustHeadroom) (*walletSweep, string) {
	plan := &walletSweep{wallet: w, account: acct, merge: true, feePaid: decimal.Zero, nativeExcess: decimal.Zero}
	needs := map[string]decimal.Decimal{}
	for _, b := range acct.Balances {
		if b.Type == "liquidity_pool_shares" || b.LiquidityPoolId != "" {
			return nil, ConsolidationSkipUnsupported
		}
		if nonZero(b.BuyingLiabilities) || nonZero(b.SellingLiabilities) {
			return nil, ConsolidationSkipUnsupported
		}
		balance := parseAmount(b.Balance)
		if b.Type == "native" || (b.Code == "" && b.Issuer == "") {
			plan.nativeExcess = balance
			continue
		}
		if b.IsAuthorized != nil && !*b.IsAuthorized && balance.IsPositive() {
			return nil, ConsolidationSkipUnsupported
		}
		plan.trustlines = append(plan.trustlines, domain.ClosedTrustline{AssetCode: b.Code, Issuer: b.Issuer})
		if balance.IsPositive() {
			key := assetKey(b.Code, b.Issuer)
			room, trusted := headroom[key]
			if !trusted || (room != nil && needs[key].Add(balance).GreaterThan(*room)) {
				return nil, ConsolidationSkipNoTrustline
			}
			needs[key] = needs[key].Add(balance)
			plan.payments = append(plan.payments, sweepPayment{assetCode: b.Code, issuer: b.Issuer, amount: balance, balanceBefore: balance})
		}
	}
	if acct.NumSponsoring > 0 || len(acct.Data) > 0 || int(acct.SubentryCount) > len(plan.trustlines) {
		return nil, ConsolidationSkipUnsupported
	}
	for _, s := range acct.Signers {
		if s.Key != acct.AccountID {
			return nil, ConsolidationSkipUnsupported
		}
	}
	if plan.opCount() > maxSweepOperations {
		return nil, ConsolidationSkipUnsupported
	}
	for key, need := range needs {
		if room := headroom[key]; room != nil {
			left := room.Sub(need)
			headroom[key] = &left
		}
	}
	return plan, ""
}

// preview turns the plan into the dry-run response.
func (p *consolidationPlan) preview(dest *domain.Wallet, merge bool, batchSize int, baseReserve decimal.Decimal) *domain.ConsolidationPreview {
	out := &domain.ConsolidationPreview{
		DestinationWallet: dest.ID, MergeSources: merge, BatchSize: batchSize,
		Batches: []domain.ConsolidationBatch{}, Wallets: p.wallets,
	}
	totalFee, recovered := int64(0), decimal.Zero
	incoming := map[string]decimal.Decimal{}
	for i, batch := range p.batches {
		out.Batches = append(out.Batches, batchSummary(i, batch, p.payers[i], p.fees[i]))
		totalFee += p.fees[i]
		for _, w := range batch {
			for _, pay := range w.payments {
				key := assetKey(pay.assetCode, pay.issuer)
				incoming[key] = incoming[key].Add(pay.amount)
			}
			if w.merge {
				recovered = recovered.Add(MinimumBalance(w.account, baseReserve))
				// The merge carries the whole remaining XLM balance.
				key := assetKey(nativeAssetCode, "")
				incoming[key] = incoming[key].Add(w.nativeExcess.Sub(w.feePaid))
			}
		}
	}
	out.TotalFeeStroops = totalFee
	out.TotalFee = stroopsToXLM(totalFee).StringFixed(stellarDecimals)
	out.ReserveRecovered = recovered.StringFixed(stellarDecimals)
	out.NetXLM = recovered.Sub(stroopsToXLM(totalFee)).StringFixed(stellarDecimals)

	before := map[string]decimal.Decimal{}
	for _, b := range p.destAcct.Balances {
		if b.Type == "liquidity_pool_shares" {
			continue
		}
		code, issuer := b.Code, b.Issuer
		if b.Type == "native" || code == "" {
			code, issuer = nativeAssetCode, ""
		}
		before[assetKey(code, issuer)] = parseAmount(b.Balance)
	}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range incoming {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		code, issuer, _ := strings.Cut(k, ":")
		out.ResultingBalances = append(out.ResultingBalances, domain.ConsolidationAssetChange{
			AssetCode: code, Issuer: issuer,
			Before: before[k].StringFixed(stellarDecimals),
			After:  before[k].Add(incoming[k]).StringFixed(stellarDecimals),
		})
	}
	return out
}

// consolidationMoves itemises what leaves one source wallet.
func consolidationMoves(p *walletSweep, baseReserve decimal.Decimal) []domain.SweepItem {
	moves := sweepItems([]*walletSweep{p})
	if p.merge {
		merged := p.nativeExcess.Sub(p.feePaid)
		moves = append(moves, domain.SweepItem{
			WalletID: p.wallet.ID, PublicKey: p.wallet.PublicKey, AssetCode: nativeAssetCode,
			Amount:        merged.StringFixed(stellarDecimals),
			BalanceBefore: p.nativeExcess.StringFixed(stellarDecimals),
			Retained:      decimal.Zero.StringFixed(stellarDecimals),
		})
	}
	return moves
}

func batchSummary(i int, batch []*walletSweep, payer *walletSweep, fee int64) domain.ConsolidationBatch {
	summary := domain.ConsolidationBatch{Index: i, FeeStroops: fee, FeePayer: payer.wallet.ID}
	for _, p := range batch {
		summary.WalletIDs = append(summary.WalletIDs, p.wallet.ID)
		summary.Operations += p.opCount()
	}
	return summary
}

func (c *Consolidator) submit(ctx context.Context, client stellar.Client, destination string, payer *walletSweep, batch []*walletSweep) (string, error) {
	var ops []txnbuild.Operation
	for _, p := range batch {
		ops = append(ops, p.operations(destination)...)
	}
	source := payer.account
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &source,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              c.baseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(60)},
	})
	if err != nil {
		return "", fmt.Errorf("build consolidation transaction: %w", err)
	}
	signer := c.signer
	if c.signerResolver != nil {
		if s := c.signerResolver.SignerForMode(ctx); s != nil {
			signer = s
		}
	}
	if signer == nil {
		return "", errors.New("no signer configured")
	}
	for _, p := range batch {
		if tx, err = signWithStoredSecret(signer, tx, p.wallet.EncryptedSecret); err != nil {
			return "", fmt.Errorf("sign for wallet %s: %w", p.wallet.ID, err)
		}
	}
	resp, err := stellar.SubmitTransactionWithContext(ctx, client, tx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", domain.ErrStellarSubmission, err)
	}
	return resp.Hash, nil
}

func (c *Consolidator) record(ctx context.Context, action string, op *domain.ConsolidationOperation) {
	if c.audit == nil {
		return
	}
	metadata := map[string]interface{}{
		"source_wallet_ids":  op.SourceWalletIDs,
		"destination_wallet": op.DestinationWallet,
		"idempotency_key":    op.IdempotencyKey,
		"merge_sources":      op.MergeSources,
		"batch_size":         op.BatchSize,
		"status":             op.Status,
		"total_fee":          op.TotalFee.String(),
		"reserve_recovered":  op.ReserveRecovered.String(),
	}
	if len(op.TransactionHashes) > 0 {
		metadata["transaction_hashes"] = op.TransactionHashes
	}
	if err := c.audit.Record(ctx, &domain.AuditEvent{
		TenantID: op.TenantID, Mode: op.Mode, ActorType: op.ActorType, ActorID: op.ActorID,
		Action: action, ResourceType: "wallet_consolidation", ResourceID: op.ID, Metadata: metadata,
	}); err != nil {
		log.Error().Err(err).Str("consolidation_id", op.ID).Msg("failed to audit consolidation")
	}
}

func (c *Consolidator) client(ctx context.Context) stellar.Client {
	if c.clientResolver != nil {
		if client := c.clientResolver.ClientForMode(ctx); client != nil {
			return client
		}
	}
	return c.stellar
}

// replay returns the operation already recorded under a key, or refuses the
// key when it was recorded for a different request.
func replay(existing *domain.ConsolidationOperation, fingerprint string) (*domain.ConsolidationOperation, bool, error) {
	if existing.Fingerprint != fingerprint {
		return nil, false, domain.ErrConsolidationKeyConflict
	}
	return existing, true, nil
}

// consolidationFingerprint hashes the request fields that determine what
// moves. Source order does not matter.
func consolidationFingerprint(req ConsolidationRequest, batchSize int) string {
	ids := append([]string(nil), req.SourceWalletIDs...)
	sort.Strings(ids)
	h := sha256.New()
	h.Write([]byte(strings.Join(ids, ",") + "|" + req.DestinationWalletID + "|" +
		strconv.FormatBool(req.MergeSources) + "|" + strconv.Itoa(batchSize)))
	return hex.EncodeToString(h.Sum(nil))
}

func walletIDs(wallets []*domain.Wallet) []string {
	ids := make([]string, len(wallets))
	for i, w := range wallets {
		ids[i] = w.ID
	}
	return ids
}
