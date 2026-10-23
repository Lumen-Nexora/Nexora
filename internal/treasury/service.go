// Package treasury monitors the platform fee wallet, computes how much of
// each asset can safely be moved to cold storage without dipping into
// Stellar's network reserve requirements, and executes/audits sweeps.
//
// It also runs the warm-wallet rebalancing engine (see rebalancer.go), which
// monitors operational balances against burn rates and produces multi-sig
// replenishment requests from cold storage.
package treasury

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/webhook"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/keypair"
	stellarnet "github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
)

// defaultBaseReserve is Stellar's per-subentry minimum balance requirement
// (currently 0.5 XLM). A funded account additionally always needs
// 2*baseReserve just to exist. The value is configurable per service instance
// because it changes the reported reserve figure.
var defaultBaseReserve = decimal.RequireFromString("0.5")

const (
	// defaultReserveCacheTTL keeps the (slow) platform-wide scan from running
	// on every reserve/sweep read.
	defaultReserveCacheTTL = 2 * time.Minute
	// defaultReserveConcurrency bounds in-flight Horizon requests during a scan.
	defaultReserveConcurrency = 16
	// defaultOffersPageLimit caps how many open offers are counted per wallet.
	// Horizon caps a single offers page at 200 records, so a wallet with more
	// than this many open offers is undercounted. This is a deliberate bound,
	// not a bug: reserve accounting must not page unboundedly per wallet.
	defaultOffersPageLimit = 200
	// reserveScanTimeout bounds one full platform-wide scan when the caller
	// supplies a context without a deadline.
	reserveScanTimeout = 2 * time.Minute
)

// AssetBalance is the fee wallet's live balance for one asset, with its USD
// equivalent when a rate is available.
type AssetBalance struct {
	Asset         string          `json:"asset"`
	Balance       decimal.Decimal `json:"balance"`
	USDEquivalent decimal.Decimal `json:"usd_equivalent"`
}

// ReserveBreakdown is the full accounting behind GetReserveRequirement.
type ReserveBreakdown struct {
	WalletCount       int             `json:"wallet_count"`
	TrustlineCount    int             `json:"trustline_count"`
	OfferCount        int             `json:"open_offers_count"`
	TotalXLMRequired  decimal.Decimal `json:"total_xlm_required"`
	CurrentXLMBalance decimal.Decimal `json:"current_xlm_balance"`
	Surplus           decimal.Decimal `json:"surplus"` // negative means deficit
	// AsOf is when this snapshot was computed. Reserve figures are a
	// point-in-time measurement, not a live guarantee.
	AsOf time.Time `json:"as_of"`
	// Cached reports whether the figures came from the short-lived cache.
	Cached bool `json:"cached"`
	// OffersPerWalletCap documents the per-wallet open-offer cap used by the
	// scan; wallets above it are undercounted.
	OffersPerWalletCap uint `json:"offers_per_wallet_cap"`
}

// FXRates resolves a spot rate between two assets. fx.Service already
// satisfies this (fx.RateResponse is a type alias for domain.RateResponse).
type FXRates interface {
	GetRates(ctx context.Context, from, to string) (*domain.RateResponse, error)
}

// BalanceAlertService evaluates wallet balances against configured thresholds.
type BalanceAlertService interface {
	EvaluateWalletBalances(ctx context.Context, walletID string, balances map[string]decimal.Decimal) error
}

type Service interface {
	GetBalances(ctx context.Context) ([]AssetBalance, error)
	GetReserveRequirement(ctx context.Context) (decimal.Decimal, error)
	GetReserveBreakdown(ctx context.Context) (*ReserveBreakdown, error)
	GetSweepableAmount(ctx context.Context, asset string) (decimal.Decimal, error)
	// ExecuteSweep validates amount against the current sweepable balance,
	// then builds, signs, and submits a payment from the fee wallet to
	// destination. It always writes a sweep_log record — including a
	// zero-amount audit row when amount is zero — and returns the Stellar
	// tx hash (empty for a zero sweep).
	ExecuteSweep(ctx context.Context, asset string, amount decimal.Decimal, destination, triggeredBy string) (string, error)
	GetConfig(ctx context.Context) ([]*Config, error)
	UpdateConfig(ctx context.Context, cfg *Config) error
	ListSweeps(ctx context.Context, limit, offset int) ([]*SweepLog, error)
}

type service struct {
	repo              Repository
	stellar           stellar.Client
	fxRates           FXRates
	webhookSvc        webhook.Service
	balanceAlertSvc   BalanceAlertService
	feeWallet         string
	network           string
	treasurySecretKey string
	usdcIssuer        string
	eurcIssuer        string

	baseReserve        decimal.Decimal
	reserveCacheTTL    time.Duration
	reserveConcurrency int
	offerspageLimit    uint

	reserveMu    sync.Mutex
	reserveCache *ReserveBreakdown
	reserveAsOf  time.Time
}

// Option customises the treasury service. Options keep NewService
// backward-compatible for callers that do not care about the tunables.
type Option func(*service)

// WithBaseReserve overrides Stellar's per-subentry base reserve. A
// non-positive value is ignored.
func WithBaseReserve(v decimal.Decimal) Option {
	return func(s *service) {
		if v.IsPositive() {
			s.baseReserve = v
		}
	}
}

// WithReserveCacheTTL sets how long a reserve snapshot is reused. A
// non-positive value disables caching.
func WithReserveCacheTTL(d time.Duration) Option {
	return func(s *service) { s.reserveCacheTTL = d }
}

// OptionsFromConfig builds the tunable options from raw configuration values.
// Malformed values are ignored so that configuration validation stays in
// internal/config rather than being duplicated here.
func OptionsFromConfig(baseReserve string, cacheTTLSeconds, concurrency int) []Option {
	opts := []Option{}
	if v, err := decimal.NewFromString(baseReserve); err == nil && v.IsPositive() {
		opts = append(opts, WithBaseReserve(v))
	}
	if cacheTTLSeconds >= 0 {
		opts = append(opts, WithReserveCacheTTL(time.Duration(cacheTTLSeconds)*time.Second))
	}
	if concurrency > 0 {
		opts = append(opts, WithReserveConcurrency(concurrency))
	}
	return opts
}

// WithReserveConcurrency bounds in-flight Horizon requests during a scan.
// Values below 1 are clamped to 1.
func WithReserveConcurrency(n int) Option {
	return func(s *service) {
		if n < 1 {
			n = 1
		}
		s.reserveConcurrency = n
	}
}

// WithBalanceAlertService attaches a wallet balance alert service to the treasury service.
func WithBalanceAlertService(svc BalanceAlertService) Option {
	return func(s *service) {
		s.balanceAlertSvc = svc
	}
}

func NewService(
	repo Repository,
	stellarClient stellar.Client,
	fxRates FXRates,
	webhookSvc webhook.Service,
	feeWallet, network, treasurySecretKey, usdcIssuer, eurcIssuer string,
	opts ...Option,
) Service {
	s := &service{
		repo:               repo,
		stellar:            stellarClient,
		fxRates:            fxRates,
		webhookSvc:         webhookSvc,
		feeWallet:          feeWallet,
		network:            network,
		treasurySecretKey:  treasurySecretKey,
		usdcIssuer:         usdcIssuer,
		eurcIssuer:         eurcIssuer,
		baseReserve:        defaultBaseReserve,
		reserveCacheTTL:    defaultReserveCacheTTL,
		reserveConcurrency: defaultReserveConcurrency,
		offerspageLimit:    defaultOffersPageLimit,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) GetBalances(ctx context.Context) ([]AssetBalance, error) {
	if s.feeWallet == "" {
		return nil, fmt.Errorf("PLATFORM_FEE_WALLET_PUBLIC_KEY is not configured")
	}

	acct, err := stellar.LoadAccountWithContext(ctx, s.stellar, s.feeWallet)
	if err != nil {
		return nil, fmt.Errorf("load fee wallet account: %w", err)
	}

	balances := make([]AssetBalance, 0, len(acct.Balances))
	for _, b := range acct.Balances {
		code := b.Code
		if code == "" {
			code = "XLM"
		}
		amt, err := decimal.NewFromString(b.Balance)
		if err != nil {
			continue
		}

		usd := decimal.Zero
		switch {
		case code == "USDC":
			usd = amt
		case s.fxRates != nil:
			if rate, err := s.fxRates.GetRates(ctx, code, "USDC"); err == nil {
				usd = amt.Mul(rate.Rate)
			}
			// No provider for this pair (e.g. XLM today) — leave USD
			// equivalent at zero rather than failing the whole call.
		}

		balances = append(balances, AssetBalance{Asset: code, Balance: amt, USDEquivalent: usd})
	}
	return balances, nil
}

// GetReserveBreakdown sums, across every wallet Nexora custodies, the XLM
// Stellar's protocol requires each account to keep locked up: a fixed
// 2*baseReserve per account plus baseReserve per trustline and per open
// offer (Stellar "subentries"). This is a platform-wide obligation, not
// specific to the fee wallet — it's what determines how much of the fee
// wallet's own XLM is actually free to sweep.
func (s *service) GetReserveBreakdown(ctx context.Context) (*ReserveBreakdown, error) {
	if cached := s.cachedReserve(); cached != nil {
		return cached, nil
	}

	scanCtx := ctx
	if _, ok := scanCtx.Deadline(); !ok {
		var cancel context.CancelFunc
		scanCtx, cancel = context.WithTimeout(scanCtx, reserveScanTimeout)
		defer cancel()
	}

	pubKeys, err := s.repo.ListWalletPublicKeys(scanCtx)
	if err != nil {
		return nil, fmt.Errorf("list wallet public keys: %w", err)
	}

	concurrency := s.reserveConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	trustlines, offers := 0, 0

	for _, pk := range pubKeys {
		if scanCtx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(pk string) {
			defer wg.Done()
			defer func() { <-sem }()

			acct, err := stellar.LoadAccountWithContext(scanCtx, s.stellar, pk)
			if err != nil {
				// A not-yet-funded or unreachable wallet is skipped rather
				// than failing the whole platform-wide calculation, and
				// because every request is bounded a single slow wallet
				// cannot stall the scan.
				return
			}
			localTrustlines := 0
			for _, b := range acct.Balances {
				if b.Code != "" {
					localTrustlines++
				}
			}
			localOffers := 0
			if offerList, err := stellar.OffersWithContext(scanCtx, s.stellar, pk, s.offersPageLimit); err == nil {
				localOffers = len(offerList)
			}

			mu.Lock()
			trustlines += localTrustlines
			offers += localOffers
			mu.Unlock()
		}(pk)
	}
	wg.Wait()

	perAccountReserve := s.baseReserve.Mul(decimal.NewFromInt(2))
	total := perAccountReserve.Mul(decimal.NewFromInt(int64(len(pubKeys)))).
		Add(s.baseReserve.Mul(decimal.NewFromInt(int64(trustlines)))).
		Add(s.baseReserve.Mul(decimal.NewFromInt(int64(offers))))

	current := decimal.Zero
	if s.feeWallet != "" {
		if acct, err := stellar.LoadAccountWithContext(scanCtx, s.stellar, s.feeWallet); err == nil {
			for _, b := range acct.Balances {
				if b.Code == "" {
					if amt, err := decimal.NewFromString(b.Balance); err == nil {
						current = amt
					}
				}
			}
		}
	}

	breakdown := &ReserveBreakdown{
		WalletCount:        len(pubKeys),
		TrustlineCount:     trustlines,
		OfferCount:         offers,
		TotalXLMRequired:   total,
		CurrentXLMBalance:  current,
		Surplus:            current.Sub(total),
		AsOf:               time.Now().UTC(),
		OffersPerWalletCap: s.offersPageLimit,
	}
	s.storeReserve(breakdown)
	return breakdown, nil
}

// cachedReserve returns a copy of the cached snapshot when it is still fresh.
func (s *service) cachedReserve() *ReserveBreakdown {
	if s.reserveCacheTTL <= 0 {
		return nil
	}
	s.reserveMu.Lock()
	defer s.reserveMu.Unlock()
	if s.reserveCache == nil {
		return nil
	}
	if time.Since(s.reserveAsOf) > s.reserveCacheTTL {
		s.reserveCache = nil
		return nil
	}
	cp := *s.reserveCache
	cp.Cached = true
	return &cp
}

func (s *service) storeReserve(bd *ReserveBreakdown) {
	if s.reserveCacheTTL <= 0 {
		return
	}
	s.reserveMu.Lock()
	cp := *bd
	cp.Cached = false
	s.reserveCache = &cp
	s.reserveAsOf = bd.AsOf
	s.reserveMu.Unlock()
}

func (s *service) GetReserveRequirement(ctx context.Context) (decimal.Decimal, error) {
	bd, err := s.GetReserveBreakdown(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	return bd.TotalXLMRequired, nil
}
