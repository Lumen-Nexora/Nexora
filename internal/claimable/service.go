package claimable

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
)

// EntryReserve is Stellar's per-subentry minimum balance requirement. A
// claimable balance is one subentry, so creating one locks 0.5 XLM — paid by
// the creator, or by the sponsor when the creation is sponsored.
var EntryReserve = decimal.RequireFromString("0.5")

// WalletResolver looks up Nexora-custodied wallets. It is declared here, and
// exchanges only plain values, so this package does not depend on
// internal/wallet.
type WalletResolver interface {
	GetByID(ctx context.Context, walletID string) (*domain.SourceWallet, error)
	// GetByPublicKey resolves a Nexora-custodied wallet from its Stellar account
	// ID, which is how a claimant or sponsor named by public key is turned into
	// something Nexora can sign for.
	GetByPublicKey(ctx context.Context, publicKey string) (*domain.SourceWallet, error)
}

type SourceWallet = domain.SourceWallet

// WebhookDispatcher is the narrow view of internal/webhook this service needs.
// Declared here so the claimable package stays independent of the webhook
// package; webhook.Dispatcher satisfies it.
type WebhookDispatcher interface {
	Dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) error
}

// CreateInput describes a new claimable balance.
type CreateInput struct {
	Asset     string
	Amount    decimal.Decimal
	Claimants []domain.Claimant

	// SourceWalletID funds the balance. When empty the service falls back to
	// its configured default source wallet.
	SourceWalletID string

	// SponsorAccount is an optional Stellar account that pays the entry reserve.
	// It must be a wallet Nexora custodies, because sponsoring requires the
	// sponsor's signature. When set, the creation is wrapped in
	// Begin/EndSponsoringFutureReserves.
	SponsorAccount string

	// RevokeOnExpiry claims an unclaimed balance back to a satisfiable custodied
	// claimant once it expires, instead of leaving it stranded.
	RevokeOnExpiry bool

	// ExpiresAt overrides the expiry derived from the claimants' absolute
	// deadlines.
	ExpiresAt *time.Time
}

// CreateResult is what the API returns from POST /v1/claimable-balances.
type CreateResult struct {
	BalanceID string
	// ReserveRequired is the XLM the entry locks up.
	ReserveRequired decimal.Decimal
	// Sponsored reports whether a sponsor paid that reserve.
	Sponsored bool
	TxHash    string
	ExpiresAt *time.Time
}

// LiveStatus is a stored balance plus its current state on Stellar.
type LiveStatus struct {
	Balance *domain.ClaimableBalance
	// OnChain is the Horizon view of the balance, nil when Horizon has no such
	// balance (already consumed, or unknown to this network).
	OnChain *horizon.ClaimableBalance
	// Claimable reports whether a claimant could claim right now.
	Claimable bool
}

// ClaimResult is the outcome of a successful claim.
type ClaimResult struct {
	BalanceID string
	ClaimedBy string
	Amount    decimal.Decimal
	TxHash    string
}

// ExpiryReport summarises one expiry-tracker pass.
type ExpiryReport struct {
	Scanned int
	Expired int
	Revoked int
	// Alerted counts tenant alerts dispatched for balances expiring within
	// the 48h warning window.
	Alerted int
}

// ExpiryWarningWindow is how far ahead of expiry a tenant is alerted.
const ExpiryWarningWindow = 48 * time.Hour

// Service is Nexora's claimable balance management layer.
type Service interface {
	Create(ctx context.Context, in CreateInput) (*CreateResult, error)
	List(ctx context.Context, f domain.ClaimableFilter) ([]*domain.ClaimableBalance, error)
	Get(ctx context.Context, id string) (*LiveStatus, error)
	Claim(ctx context.Context, id, claimantAccount string) (*ClaimResult, error)
	// ProcessExpired is the background expiry tracker's entry point.
	ProcessExpired(ctx context.Context) (*ExpiryReport, error)
}

type service struct {
	repo         Repository
	stellar      stellar.Client
	claimables   stellar.ClaimableBalanceClient
	signer       stellar.Signer
	wallets      WalletResolver
	webhooks     WebhookDispatcher
	assetIssuers map[string]string
	// defaultSourceWalletID funds a creation whose request did not name a
	// source wallet.
	defaultSourceWalletID string
}

func NewService(
	repo Repository,
	stellarClient stellar.Client,
	claimableClient stellar.ClaimableBalanceClient,
	signer stellar.Signer,
	wallets WalletResolver,
	webhooks WebhookDispatcher,
	defaultSourceWalletID string,
	assetIssuers map[string]string,
) Service {
	return &service{
		repo:                  repo,
		stellar:               stellarClient,
		claimables:            claimableClient,
		signer:                signer,
		wallets:               wallets,
		webhooks:              webhooks,
		defaultSourceWalletID: defaultSourceWalletID,
		assetIssuers:          assetIssuers,
	}
}

func (s *service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	if len(in.Claimants) == 0 {
		return nil, domain.ErrNoClaimants
	}
	if !in.Amount.GreaterThan(decimal.Zero) {
		return nil, domain.ErrInvalidAmount
	}

	srcWallet, err := s.resolveSourceWallet(ctx, in.SourceWalletID)
	if err != nil {
		return nil, err
	}

	// The sponsor has to be signable, so a sponsor account that Nexora does not
	// custody is rejected up front rather than at signature time.
	var sponsorWallet *SourceWallet
	if in.SponsorAccount != "" {
		sponsorWallet, err = s.wallets.GetByPublicKey(ctx, in.SponsorAccount)
		if err != nil || sponsorWallet == nil {
			return nil, fmt.Errorf("%w: %s", domain.ErrSponsorNotCustodied, in.SponsorAccount)
		}
	}

	asset, err := s.buildAsset(in.Asset)
	if err != nil {
		return nil, err
	}

	destinations := make([]txnbuild.Claimant, 0, len(in.Claimants))
	for _, c := range in.Claimants {
		predicate, err := PredicateToXDR(c.Predicate)
		if err != nil {
			return nil, err
		}
		destinations = append(destinations, txnbuild.Claimant{Destination: c.Account, Predicate: predicate})
	}

	createOp := &txnbuild.CreateClaimableBalance{
		Amount:       in.Amount.StringFixed(7),
		Asset:        asset,
		Destinations: destinations,
	}

	ops := []txnbuild.Operation{createOp}
	createOpIndex := uint32(0)
	if sponsorWallet != nil {
		// Begin sponsoring must open the transaction and end sponsoring must
		// close it, with the sponsored work in between — so the pair wraps the
		// create operation rather than sitting beside it. The creator is the
		// sponsored account: it is the one whose balance would otherwise lock
		// the entry reserve.
		ops = []txnbuild.Operation{
			&txnbuild.BeginSponsoringFutureReserves{
				SponsoredID:   srcWallet.PublicKey,
				SourceAccount: sponsorWallet.PublicKey,
			},
			createOp,
			&txnbuild.EndSponsoringFutureReserves{SourceAccount: sponsorWallet.PublicKey},
		}
		createOpIndex = 1
	}

	srcAccount, err := s.stellar.LoadAccount(srcWallet.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("load source wallet account: %w", err)
	}

	stellarTx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &srcAccount,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              txnbuild.MinBaseFee * int64(len(ops)),
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(30),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build create claimable balance transaction: %w", err)
	}

	// Both the creator and the sponsor must sign a sponsored creation; an
	// unsponsored one is signed by the creator alone.
	signers := []*SourceWallet{srcWallet}
	if sponsorWallet != nil {
		signers = append(signers, sponsorWallet)
	}
	stellarTx, err = s.sign(stellarTx, signers...)
	if err != nil {
		return nil, err
	}

	balanceID, err := BalanceID(stellarTx.SourceAccount().AccountID, stellarTx.SequenceNumber(), createOpIndex)
	if err != nil {
		return nil, err
	}

	resp, err := s.stellar.SubmitTransaction(stellarTx)
	if err != nil {
		return nil, fmt.Errorf("submit create claimable balance: %w", err)
	}

	now := time.Now().UTC()
	expiresAt := in.ExpiresAt
	if expiresAt == nil {
		expiresAt = EarliestDeadline(in.Claimants)
	}

	record := &domain.ClaimableBalance{
		ID:             balanceID,
		TenantID:       tenantPtr(ctx),
		Asset:          in.Asset,
		Amount:         in.Amount,
		Claimants:      in.Claimants,
		Sponsor:        in.SponsorAccount,
		Status:         domain.ClaimableBalanceStatusPending,
		RevokeOnExpiry: in.RevokeOnExpiry,
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
	}

	if err := s.repo.Create(ctx, record); err != nil {
		// The balance exists on chain; the ID is logged so the row can be
		// reconstructed rather than the funds being orphaned silently.
		log.Error().Err(err).Str("balance_id", balanceID).Str("tx_hash", resp.Hash).
			Msg("claimable: created on chain but failed to persist")
		return nil, fmt.Errorf("persist claimable balance %s: %w", balanceID, err)
	}

	s.dispatch(ctx, domain.EventClaimableBalanceCreated, map[string]interface{}{
		"balance_id": balanceID,
		"asset":      in.Asset,
		"amount":     in.Amount.StringFixed(7),
		"sponsor":    in.SponsorAccount,
		"tx_hash":    resp.Hash,
		"expires_at": formatTime(expiresAt),
	})

	return &CreateResult{
		BalanceID:       balanceID,
		ReserveRequired: EntryReserve,
		Sponsored:       sponsorWallet != nil,
		TxHash:          resp.Hash,
		ExpiresAt:       expiresAt,
	}, nil
}

func (s *service) List(ctx context.Context, f domain.ClaimableFilter) ([]*domain.ClaimableBalance, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return s.repo.List(ctx, f)
}

func (s *service) Get(ctx context.Context, id string) (*LiveStatus, error) {
	balance, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	live := &LiveStatus{Balance: balance}

	if s.claimables != nil {
		onChain, err := s.claimables.ClaimableBalance(id)
		switch {
		case err == nil:
			live.OnChain = &onChain
		case isNotFound(err):
			// Consumed or not on this network — the local row still stands.
		default:
			log.Warn().Err(err).Str("balance_id", id).
				Msg("claimable: could not read live status from Horizon")
		}
	}

	now := time.Now().UTC()
	if balance.Status == domain.ClaimableBalanceStatusPending && !expiredAt(balance, now) {
		live.Claimable = AnyClaimantSatisfiable(balance.Claimants, now, balance.CreatedAt)
	}

	return live, nil
}

func (s *service) Claim(ctx context.Context, id, claimantAccount string) (*ClaimResult, error) {
	balance, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if balance.Status != domain.ClaimableBalanceStatusPending {
		return nil, domain.ErrClaimableBalanceNotPending
	}

	claimant, err := s.resolveClaimant(ctx, balance, claimantAccount)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	// Pre-flight the predicate locally and refuse before touching Horizon: a
	// claim that cannot succeed would still burn a round trip, and on some
	// networks a fee.
	if !Satisfiable(claimant.Predicate, now, balance.CreatedAt) {
		return nil, domain.ErrPredicateNotSatisfiable
	}
	if expiredAt(balance, now) {
		return nil, domain.ErrClaimableBalanceNotPending
	}

	wallet, err := s.wallets.GetByPublicKey(ctx, claimant.Account)
	if err != nil || wallet == nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrClaimantNotCustodied, claimant.Account)
	}

	if err := s.repo.MarkClaimed(ctx, balance.ID, claimant.Account, now); err != nil {
		return nil, err
	}

	txHash, err := s.submitClaim(ctx, balance.ID, wallet)
	if err != nil {
		_ = s.repo.MarkExpired(ctx, balance.ID, now)
		return nil, err
	}

	s.dispatch(ctx, domain.EventClaimableBalanceClaimed, map[string]interface{}{
		"balance_id": balance.ID,
		"asset":      balance.Asset,
		"amount":     balance.Amount.StringFixed(7),
		"claimed_by": claimant.Account,
		"tx_hash":    txHash,
	})

	return &ClaimResult{
		BalanceID: balance.ID,
		ClaimedBy: claimant.Account,
		Amount:    balance.Amount,
		TxHash:    txHash,
	}, nil
}

// ProcessExpired is the background expiry tracker. It scans pending balances,
// alerts tenants about balances expiring within ExpiryWarningWindow, and for
// expired balances either reclaims them (ClaimClaimableBalance by a custodied
// satisfiable claimant, returning funds to the tenant treasury) or marks them
// expired when no reclaim path exists.
func (s *service) ProcessExpired(ctx context.Context) (*ExpiryReport, error) {
	now := time.Now().UTC()
	pending, err := s.repo.ListPending(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending claimable balances: %w", err)
	}

	report := &ExpiryReport{Scanned: len(pending)}

	for _, b := range pending {
		if b.ExpiresAt == nil {
			continue
		}

		if !expiredAt(b, now) {
			if ExpiresWithin(b, now, ExpiryWarningWindow) {
				s.dispatch(ctx, domain.EventClaimableBalanceExpiring, map[string]interface{}{
					"balance_id": b.ID,
					"asset":      b.Asset,
					"amount":     b.Amount.StringFixed(7),
					"expires_at": formatTime(b.ExpiresAt),
				})
				report.Alerted++
			}
			continue
		}

		if err := s.repo.MarkExpired(ctx, b.ID, now); err != nil {
			log.Error().Err(err).Str("balance_id", b.ID).Msg("claimable: mark expired failed")
			continue
		}
		report.Expired++

		if !b.RevokeOnExpiry {
			continue
		}

		revoked, err := s.reclaimExpired(ctx, b, now)
		if err != nil {
			log.Error().Err(err).Str("balance_id", b.ID).Msg("claimable: reclaim of expired balance failed")
			continue
		}
		if revoked {
			report.Revoked++
		}
	}

	return report, nil
}

// reclaimExpired claims an expired balance back to a custodied claimant whose
// predicate is satisfiable now, returning funds to the tenant treasury. It
// reports false when no custodied claimant can take the funds.
func (s *service) reclaimExpired(ctx context.Context, b *domain.ClaimableBalance, now time.Time) (bool, error) {
	for _, c := range b.Claimants {
		if !Satisfiable(c.Predicate, now, b.CreatedAt) {
			continue
		}
		wallet, err := s.wallets.GetByPublicKey(ctx, c.Account)
		if err != nil || wallet == nil {
			continue
		}
		txHash, err := s.submitClaim(ctx, b.ID, wallet)
		if err != nil {
			return false, err
		}
		if err := s.repo.MarkClaimed(ctx, b.ID, c.Account, now); err != nil {
			log.Error().Err(err).Str("balance_id", b.ID).Msg("claimable: reclaimed on chain but failed to record")
		}
		s.dispatch(ctx, domain.EventClaimableBalanceClaimed, map[string]interface{}{
			"balance_id": b.ID,
			"asset":      b.Asset,
			"amount":     b.Amount.StringFixed(7),
			"claimed_by": c.Account,
			"reclaimed":  true,
			"tx_hash":    txHash,
		})
		return true, nil
	}
	return false, nil
}

// ExpiresWithin reports whether a balance's expiry falls inside (now, now+window].
func ExpiresWithin(b *domain.ClaimableBalance, now time.Time, window time.Duration) bool {
	if b.ExpiresAt == nil {
		return false
	}
	remaining := b.ExpiresAt.Sub(now)
	return remaining > 0 && remaining <= window
}

func (s *service) resolveSourceWallet(ctx context.Context, id string) (*SourceWallet, error) {
	if id == "" {
		id = s.defaultSourceWalletID
	}
	w, err := s.wallets.GetByID(ctx, id)
	if err != nil || w == nil {
		return nil, fmt.Errorf("source wallet %q: %w", id, domain.ErrWalletNotFound)
	}
	return w, nil
}

func tenantPtr(ctx context.Context) *string {
	id := tenant.IDFromContext(ctx)
	if id == "" {
		return nil
	}
	return &id
}

func (s *service) dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) {
	if s.webhooks == nil {
		return
	}
	if err := s.webhooks.Dispatch(ctx, eventType, payload); err != nil {
		log.Warn().Err(err).Str("event", string(eventType)).Msg("claimable: webhook dispatch failed")
	}
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

var _ = errors.New
var _ = hex.EncodeToString
