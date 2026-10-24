package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/strkey"
	"github.com/stellar/go/txnbuild"
)

// maxClosureOperations is Stellar's per-transaction operation limit. A closure
// is one atomic transaction: one ChangeTrust per trustline plus the merge.
const maxClosureOperations = 100

// minJustificationLength keeps "x" or "ok" out of a permanent compliance
// record; a reviewer must be able to understand why the account was destroyed.
const minJustificationLength = 10

// Audit actions written by the closure protocol.
const (
	AuditActionClosureInitiated = "wallet.closure.initiated"
	AuditActionClosureCompleted = "wallet.closure.completed"
	AuditActionClosureFailed    = "wallet.closure.failed"
	AuditActionCustodySettings  = "wallet.custody_settings.updated"
)

// AuditRecorder writes a permanent tenant audit event. audit.Service
// satisfies it.
type AuditRecorder interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
}

// CloseRequest is an operator's request to destroy a custodial account.
type CloseRequest struct {
	WalletID string
	// Justification is stored verbatim in the closure record and audit log.
	Justification string
	// Confirm must equal the wallet's public key. It makes the destructive
	// call impossible to issue by accident or by replaying a request body
	// against a different wallet ID.
	Confirm string
}

// Closer runs the destructive account closure protocol (#96): it proves a
// custodial account holds nothing but native XLM, removes every asset
// trustline, and merges the account into the tenant master address so the
// locked minimum reserves are recovered.
//
// Removal and merge are one Stellar transaction. Operations apply in order, so
// each trustline is gone before AccountMerge runs, and if any operation fails
// the ledger is left untouched.
type Closer struct {
	wallets        Repository
	closures       ClosureRepository
	stellar        stellar.Client
	clientResolver stellar.ClientResolver
	signer         stellar.Signer
	signerResolver stellar.SignerResolver
	audit          AuditRecorder
}

// NewCloser constructs the closure protocol. signer signs with the wallet's
// stored secret; audit is mandatory because a closure without a compliance
// record must never reach the ledger.
func NewCloser(wallets Repository, closures ClosureRepository, client stellar.Client, signer stellar.Signer, audit AuditRecorder) *Closer {
	return &Closer{wallets: wallets, closures: closures, stellar: client, signer: signer, audit: audit}
}

// WithClientResolver routes Horizon calls by the request's live/test mode.
func (c *Closer) WithClientResolver(resolver stellar.ClientResolver) *Closer {
	c.clientResolver = resolver
	return c
}

// WithSignerResolver routes signing by the request's live/test mode.
func (c *Closer) WithSignerResolver(resolver stellar.SignerResolver) *Closer {
	c.signerResolver = resolver
	return c
}

// Close validates and executes a closure. Every refusal happens before any
// state is written; once the closure row exists the outcome is always
// recorded, as completed with the transaction hash or failed with the reason.
func (c *Closer) Close(ctx context.Context, req CloseRequest) (*domain.AccountClosure, error) {
	tenantID := tenant.IDFromContext(ctx)
	actorType, actorID := closureOperator(ctx)
	if tenantID == "" || actorID == "" {
		return nil, domain.ErrClosureOperatorRequired
	}
	justification := strings.TrimSpace(req.Justification)
	if len(justification) < minJustificationLength {
		return nil, fmt.Errorf("%w: at least %d characters", domain.ErrClosureJustificationRequired, minJustificationLength)
	}

	w, err := c.wallets.GetByID(ctx, req.WalletID)
	if err != nil {
		return nil, err
	}
	// The repository already scopes by tenant; this guards a repository that
	// does not (or a context without a tenant) from closing a foreign wallet.
	if w.TenantID == nil || *w.TenantID != tenantID {
		return nil, domain.ErrWalletNotFound
	}
	if req.Confirm != w.PublicKey {
		return nil, domain.ErrClosureConfirmationMismatch
	}
	if w.CustodyType != "" && w.CustodyType != domain.CustodyCustodial {
		return nil, domain.ErrClosureNotCustodial
	}

	mode := tenant.ModeOrDefault(ctx, domain.ModeLive)
	settings, err := c.closures.GetCustodySettings(ctx, tenantID, mode)
	if err != nil {
		return nil, err
	}
	if settings.PrimarySettlementWalletID != nil && *settings.PrimarySettlementWalletID == w.ID {
		return nil, domain.ErrClosurePrimarySettlement
	}
	if settings.MasterAddress == w.PublicKey {
		return nil, domain.ErrClosureMasterAccount
	}

	pending, err := c.closures.CountPendingTransfers(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	if pending > 0 {
		return nil, fmt.Errorf("%w: %d in flight", domain.ErrClosurePendingTransfers, pending)
	}

	client := c.client(ctx)
	acct, err := stellar.LoadAccountWithContext(ctx, client, w.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("load account: %w", err)
	}
	plan, err := planClosure(acct)
	if err != nil {
		return nil, err
	}
	// AccountMerge into a missing account fails on-chain with op_no_destination;
	// refuse up front with a clear error instead of spending a fee.
	if _, err := stellar.LoadAccountWithContext(ctx, client, settings.MasterAddress); err != nil {
		if stellar.IsNotFound(err) {
			return nil, domain.ErrClosureMasterAddressNotFunded
		}
		return nil, fmt.Errorf("load master address: %w", err)
	}

	native := plan.nativeBalance
	closure := &domain.AccountClosure{
		TenantID:           tenantID,
		Mode:               mode,
		WalletID:           w.ID,
		DestinationAddress: settings.MasterAddress,
		ActorType:          actorType,
		ActorID:            actorID,
		Justification:      justification,
		RemovedTrustlines:  plan.trustlines,
		NativeBalance:      &native,
	}
	if master, err := c.wallets.GetByPublicKey(ctx, settings.MasterAddress); err == nil {
		closure.DestinationWallet = &master.ID
	}
	if err := c.closures.Create(ctx, closure); err != nil {
		return nil, err
	}

	// The compliance record is written before the transaction is submitted
	// and the closure fails closed without it: an account must never be
	// destroyed without a permanent trail of who did it and why.
	if err := c.record(ctx, AuditActionClosureInitiated, closure, nil); err != nil {
		return c.fail(ctx, closure, fmt.Errorf("write compliance audit log: %w", err))
	}

	tx, err := buildClosureTx(&acct, plan, settings.MasterAddress)
	if err != nil {
		return c.fail(ctx, closure, err)
	}
	signed, err := c.sign(ctx, tx, w.EncryptedSecret)
	if err != nil {
		return c.fail(ctx, closure, fmt.Errorf("sign closure transaction: %w", err))
	}
	resp, err := stellar.SubmitTransactionWithContext(ctx, client, signed)
	if err != nil {
		return c.fail(ctx, closure, fmt.Errorf("%w: %v", domain.ErrStellarSubmission, err))
	}

	// The account no longer exists on-chain. Persisting the outcome is best
	// effort from here: failing the request would invite a retry of an
	// irreversible operation, so errors are logged loudly instead.
	hash := resp.Hash
	closure.TransactionHash = &hash
	closure.Status = domain.ClosureStatusCompleted
	if err := c.closures.Complete(ctx, closure.ID, hash); err != nil {
		log.Error().Err(err).Str("closure_id", closure.ID).Str("tx_hash", hash).
			Msg("account merged on-chain but closure record was not completed")
	}
	if err := c.record(ctx, AuditActionClosureCompleted, closure, nil); err != nil {
		log.Error().Err(err).Str("closure_id", closure.ID).Str("tx_hash", hash).
			Msg("account merged on-chain but completion audit event was not written")
	}
	return closure, nil
}

// Get returns one of the calling tenant's closures.
func (c *Closer) Get(ctx context.Context, id string) (*domain.AccountClosure, error) {
	return c.closures.Get(ctx, id, tenant.IDFromContext(ctx))
}

// List returns the calling tenant's closures, newest first.
func (c *Closer) List(ctx context.Context, limit, offset int) ([]*domain.AccountClosure, error) {
	return c.closures.List(ctx, tenant.IDFromContext(ctx), limit, offset)
}

// GetCustodySettings returns the calling tenant's settings for the request mode.
func (c *Closer) GetCustodySettings(ctx context.Context) (*domain.TenantCustodySettings, error) {
	return c.closures.GetCustodySettings(ctx, tenant.IDFromContext(ctx), tenant.ModeOrDefault(ctx, domain.ModeLive))
}

// CustodySettingsInput is the full replacement for a tenant's custody
// settings in the request mode.
type CustodySettingsInput struct {
	MasterAddress             string
	PrimarySettlementWalletID *string
	AutoSweepEnabled          bool
}

// SetCustodySettings validates and stores the master address, primary
// settlement wallet and sweep opt-in for the calling tenant and request mode.
func (c *Closer) SetCustodySettings(ctx context.Context, in CustodySettingsInput) (*domain.TenantCustodySettings, error) {
	masterAddress, primarySettlementWalletID := in.MasterAddress, in.PrimarySettlementWalletID
	tenantID := tenant.IDFromContext(ctx)
	_, actorID := closureOperator(ctx)
	if tenantID == "" || actorID == "" {
		return nil, domain.ErrClosureOperatorRequired
	}
	if !strkey.IsValidEd25519PublicKey(masterAddress) {
		return nil, domain.ErrInvalidMasterAddress
	}
	if primarySettlementWalletID != nil && *primarySettlementWalletID != "" {
		w, err := c.wallets.GetByID(ctx, *primarySettlementWalletID)
		if err != nil {
			return nil, err
		}
		if w.TenantID == nil || *w.TenantID != tenantID {
			return nil, domain.ErrWalletNotFound
		}
	} else {
		primarySettlementWalletID = nil
	}

	settings := &domain.TenantCustodySettings{
		TenantID:                  tenantID,
		Mode:                      tenant.ModeOrDefault(ctx, domain.ModeLive),
		MasterAddress:             masterAddress,
		PrimarySettlementWalletID: primarySettlementWalletID,
		AutoSweepEnabled:          in.AutoSweepEnabled,
		UpdatedBy:                 actorID,
	}
	if err := c.closures.UpsertCustodySettings(ctx, settings); err != nil {
		return nil, err
	}
	metadata := map[string]interface{}{"master_address": masterAddress, "auto_sweep_enabled": in.AutoSweepEnabled}
	if primarySettlementWalletID != nil {
		metadata["primary_settlement_wallet_id"] = *primarySettlementWalletID
	}
	if err := c.audit.Record(ctx, &domain.AuditEvent{
		Action:       AuditActionCustodySettings,
		ResourceType: "tenant_custody_settings",
		ResourceID:   tenantID,
		Metadata:     metadata,
	}); err != nil {
		log.Error().Err(err).Str("tenant_id", tenantID).Msg("failed to audit custody settings change")
	}
	return settings, nil
}

// closurePlan is the validated on-chain state of an account about to be merged.
type closurePlan struct {
	nativeBalance decimal.Decimal
	trustlines    []domain.ClosedTrustline
}

// planClosure proves the account can be merged without losing funds: every
// non-native trustline is empty with no open liabilities, and nothing other
// than those trustlines (offers, data entries, extra signers, sponsorships)
// would block AccountMerge.
func planClosure(acct horizon.Account) (closurePlan, error) {
	plan := closurePlan{trustlines: []domain.ClosedTrustline{}}

	for _, b := range acct.Balances {
		if b.Type == "liquidity_pool_shares" || b.LiquidityPoolId != "" {
			return plan, fmt.Errorf("%w: liquidity pool share trustline %s", domain.ErrClosureUnsupportedAccount, b.LiquidityPoolId)
		}
		if b.Type == "native" || (b.Code == "" && b.Issuer == "") {
			bal, err := decimal.NewFromString(b.Balance)
			if err != nil {
				return plan, fmt.Errorf("parse native balance: %w", err)
			}
			plan.nativeBalance = bal
			// Native liabilities mean an open offer is reserving XLM.
			if nonZero(b.BuyingLiabilities) || nonZero(b.SellingLiabilities) {
				return plan, fmt.Errorf("%w: open offers hold native liabilities", domain.ErrClosureUnsupportedAccount)
			}
			continue
		}
		if nonZero(b.Balance) || nonZero(b.BuyingLiabilities) || nonZero(b.SellingLiabilities) {
			return plan, fmt.Errorf("%w: %s:%s balance %s", domain.ErrClosureOutstandingBalance, b.Code, b.Issuer, b.Balance)
		}
		plan.trustlines = append(plan.trustlines, domain.ClosedTrustline{AssetCode: b.Code, Issuer: b.Issuer})
	}

	if acct.NumSponsoring > 0 {
		return plan, fmt.Errorf("%w: account sponsors %d ledger entries", domain.ErrClosureUnsupportedAccount, acct.NumSponsoring)
	}
	if len(acct.Data) > 0 {
		return plan, fmt.Errorf("%w: account has %d data entries", domain.ErrClosureUnsupportedAccount, len(acct.Data))
	}
	for _, s := range acct.Signers {
		if s.Key != acct.AccountID {
			return plan, fmt.Errorf("%w: account has additional signers", domain.ErrClosureUnsupportedAccount)
		}
	}
	// Anything counted as a subentry that is not one of the trustlines above
	// (an offer, typically) would make AccountMerge fail with
	// op_has_sub_entries.
	if int(acct.SubentryCount) > len(plan.trustlines) {
		return plan, fmt.Errorf("%w: %d subentries besides trustlines", domain.ErrClosureUnsupportedAccount, int(acct.SubentryCount)-len(plan.trustlines))
	}
	if len(plan.trustlines)+1 > maxClosureOperations {
		return plan, fmt.Errorf("%w: %d trustlines exceed one transaction", domain.ErrClosureUnsupportedAccount, len(plan.trustlines))
	}
	return plan, nil
}

// buildClosureTx removes every trustline and then merges the account into
// destination, in that order, as one transaction.
func buildClosureTx(acct *horizon.Account, plan closurePlan, destination string) (*txnbuild.Transaction, error) {
	ops := make([]txnbuild.Operation, 0, len(plan.trustlines)+1)
	for _, tl := range plan.trustlines {
		line, err := txnbuild.CreditAsset{Code: tl.AssetCode, Issuer: tl.Issuer}.ToChangeTrustAsset()
		if err != nil {
			return nil, fmt.Errorf("build change trust asset %s: %w", tl.AssetCode, err)
		}
		ops = append(ops, &txnbuild.ChangeTrust{Line: line, Limit: "0"})
	}
	ops = append(ops, &txnbuild.AccountMerge{Destination: destination})

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        acct,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(30)},
	})
	if err != nil {
		return nil, fmt.Errorf("build closure transaction: %w", err)
	}
	return tx, nil
}

func (c *Closer) fail(ctx context.Context, closure *domain.AccountClosure, cause error) (*domain.AccountClosure, error) {
	msg := cause.Error()
	closure.Status = domain.ClosureStatusFailed
	closure.ErrorMessage = &msg
	if err := c.closures.Fail(ctx, closure.ID, cause); err != nil {
		log.Error().Err(err).Str("closure_id", closure.ID).Msg("failed to record closure failure")
	}
	if err := c.record(ctx, AuditActionClosureFailed, closure, cause); err != nil {
		log.Error().Err(err).Str("closure_id", closure.ID).Msg("failed to audit closure failure")
	}
	return closure, cause
}

func (c *Closer) record(ctx context.Context, action string, closure *domain.AccountClosure, cause error) error {
	metadata := map[string]interface{}{
		"wallet_id":           closure.WalletID,
		"operator_type":       closure.ActorType,
		"operator_id":         closure.ActorID,
		"justification":       closure.Justification,
		"destination_address": closure.DestinationAddress,
		"removed_trustlines":  closure.RemovedTrustlines,
	}
	if closure.NativeBalance != nil {
		metadata["native_balance"] = closure.NativeBalance.String()
	}
	if closure.TransactionHash != nil {
		metadata["transaction_hash"] = *closure.TransactionHash
	}
	if cause != nil {
		metadata["error"] = cause.Error()
	}
	return c.audit.Record(ctx, &domain.AuditEvent{
		TenantID:     closure.TenantID,
		Mode:         closure.Mode,
		ActorType:    closure.ActorType,
		ActorID:      closure.ActorID,
		Action:       action,
		ResourceType: "account_closure",
		ResourceID:   closure.ID,
		Metadata:     metadata,
	})
}

func (c *Closer) client(ctx context.Context) stellar.Client {
	if c.clientResolver != nil {
		if client := c.clientResolver.ClientForMode(ctx); client != nil {
			return client
		}
	}
	return c.stellar
}

func (c *Closer) sign(ctx context.Context, tx *txnbuild.Transaction, encryptedSecret string) (*txnbuild.Transaction, error) {
	signer := c.signer
	if c.signerResolver != nil {
		if s := c.signerResolver.SignerForMode(ctx); s != nil {
			signer = s
		}
	}
	if signer == nil {
		return nil, errors.New("no signer configured")
	}
	return signWithStoredSecret(signer, tx, encryptedSecret)
}

// closureOperator identifies the authenticated principal performing the
// closure. It never trusts a caller-supplied operator ID.
func closureOperator(ctx context.Context) (actorType, actorID string) {
	if uID := tenant.UserIDFromContext(ctx); uID != "" {
		return "user", uID
	}
	if kID := tenant.APIKeyIDFromContext(ctx); kID != "" {
		return "api_key", kID
	}
	return "", ""
}

func nonZero(amount string) bool {
	if amount == "" {
		return false
	}
	d, err := decimal.NewFromString(amount)
	// An unparseable amount is treated as non-zero so it refuses the closure.
	return err != nil || !d.IsZero()
}
