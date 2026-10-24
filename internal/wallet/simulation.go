package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/strkey"
)

// SimulationRequest describes one classic payment, before signing. Sequence is
// optional; when present it must equal the predicted next account sequence.
type SimulationRequest struct {
	Destination string `json:"destination"`
	Asset       string `json:"asset"`
	Issuer      string `json:"issuer,omitempty"`
	Amount      string `json:"amount"`
	Sequence    string `json:"sequence,omitempty"`
}

type SimulationResult struct {
	Success         bool   `json:"success"`
	Code            string `json:"code"`
	EstimatedFee    string `json:"estimated_fee_stroops"`
	Sequence        string `json:"sequence"`
	Ledger          uint32 `json:"ledger"`
	Cached          bool   `json:"cached"`
	StateCacheTTLMS int64  `json:"state_cache_ttl_ms"`
	Warning         string `json:"warning"`
}

type SimulationService interface {
	Simulate(context.Context, string, SimulationRequest) (*SimulationResult, error)
}

type SimulationWalletReader interface {
	GetByID(context.Context, string) (*domain.Wallet, error)
}
type SimulationStateReader interface {
	Account(context.Context, string) (stellar.PreflightAccount, error)
	Network(context.Context) (stellar.PreflightNetwork, error)
}

type simulationService struct {
	wallets    SimulationWalletReader
	live, test SimulationStateReader
	cache      redis.UniversalClient
	namespace  string
}

// namespace must identify this deployment's live/test Horizon endpoints so
// Redis snapshots can never cross networks or private test chains.
func NewSimulationService(wallets SimulationWalletReader, live, test SimulationStateReader, cache redis.UniversalClient, namespace string) SimulationService {
	return &simulationService{wallets: wallets, live: live, test: test, cache: cache, namespace: fmt.Sprintf("%x", sha256.Sum256([]byte(namespace)))}
}

var _ SimulationService = (*simulationService)(nil)
var _ SimulationStateReader = (*stellar.PreflightClient)(nil)

const simulationTTL = 2 * time.Second

// Cache failures fall back to Horizon, never to expired account state. Cache
// access is bounded so an unavailable Redis does not consume the HTTP budget.
func (s *simulationService) state(ctx context.Context, key string, out interface{}, load func() error) (bool, error) {
	if s.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		raw, err := s.cache.Get(cacheCtx, key).Bytes()
		cancel()
		if err == nil {
			if decodeErr := json.Unmarshal(raw, out); decodeErr == nil {
				return true, nil
			} else {
				log.Debug().Err(decodeErr).Msg("invalid preflight cache entry; reading Horizon")
			}
		} else if !errors.Is(err, redis.Nil) {
			log.Debug().Err(err).Msg("preflight cache unavailable; reading Horizon")
		}
	}
	if err := load(); err != nil {
		return false, err
	}
	if s.cache != nil {
		raw, err := json.Marshal(out)
		if err != nil {
			return false, fmt.Errorf("encode simulation cache: %w", err)
		}
		cacheCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		if err := s.cache.Set(cacheCtx, key, raw, simulationTTL).Err(); err != nil {
			log.Debug().Err(err).Msg("preflight cache write failed")
		}
		cancel()
	}
	return false, nil
}

func (s *simulationService) Simulate(ctx context.Context, id string, req SimulationRequest) (*SimulationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("simulate: %w", err)
	}
	w, err := s.wallets.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("simulation wallet: %w", err)
	}
	if w == nil {
		return nil, domain.ErrWalletNotFound
	}
	mode := tenant.ModeOrDefault(ctx, domain.ModeLive)
	if w.Mode != mode || (w.TenantID != nil && *w.TenantID != tenant.IDFromContext(ctx)) {
		return nil, domain.ErrWalletNotFound
	}
	if w.CustodyType == domain.CustodyContract || w.ContractID != "" {
		return nil, fmt.Errorf("%w: classic custodial payments only", domain.ErrInvalidAsset)
	}
	if !strkey.IsValidEd25519PublicKey(req.Destination) || !strkey.IsValidEd25519PublicKey(w.PublicKey) {
		return nil, fmt.Errorf("%w: invalid account address", domain.ErrInvalidAsset)
	}
	if req.Destination == w.PublicKey {
		return nil, domain.ErrSelfTransfer
	}
	amount, err := simulationAmount(req.Amount)
	if err != nil || !amount.IsPositive() {
		return nil, domain.ErrInvalidAmount
	}
	native := req.Asset == "XLM" || req.Asset == ""
	if native && req.Issuer != "" {
		return nil, domain.ErrInvalidAsset
	}
	if !native {
		if len(req.Asset) < 1 || len(req.Asset) > 12 || !strkey.IsValidEd25519PublicKey(req.Issuer) {
			return nil, domain.ErrInvalidAsset
		}
		for _, c := range req.Asset {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				return nil, domain.ErrInvalidAsset
			}
		}
	}
	reader := s.live
	if mode == domain.ModeTest {
		reader = s.test
	}
	if reader == nil {
		return nil, fmt.Errorf("simulation network is not configured")
	}
	prefix := "nexora:preflight:v1:" + s.namespace + ":" + string(mode) + ":"
	var net stellar.PreflightNetwork
	netHit, err := s.state(ctx, prefix+"network", &net, func() error { var e error; net, e = reader.Network(ctx); return e })
	if err != nil {
		return nil, fmt.Errorf("simulation network state: %w", err)
	}
	if net.Fee <= 0 || net.BaseReserve <= 0 || net.Ledger == 0 {
		return nil, fmt.Errorf("invalid simulation network state")
	}
	result := &SimulationResult{Code: "ok", EstimatedFee: strconv.FormatInt(net.Fee, 10), Ledger: net.Ledger, StateCacheTTLMS: simulationTTL.Milliseconds(), Warning: "Local classic-payment pre-flight only; not network execution or signature validation. State and fees may change. Reload the source sequence and balances before signing; this does not reserve a sequence."}
	fail := func(code string) (*SimulationResult, error) { result.Code = code; return result, nil }
	var source, dest stellar.PreflightAccount
	sourceHit, err := s.state(ctx, prefix+w.PublicKey, &source, func() error { var e error; source, e = reader.Account(ctx, w.PublicKey); return e })
	if errors.Is(err, stellar.ErrPreflightAccountMissing) {
		return fail("source_missing")
	}
	if err != nil {
		return nil, fmt.Errorf("simulation source: %w", err)
	}
	seq, err := strconv.ParseInt(source.Sequence, 10, 64)
	if err != nil || seq < 0 || seq == math.MaxInt64 || source.AccountID != w.PublicKey {
		return nil, fmt.Errorf("invalid simulation source state")
	}
	result.Sequence = strconv.FormatInt(seq+1, 10)
	if req.Sequence != "" && req.Sequence != result.Sequence {
		return fail("bad_sequence")
	}
	destHit, err := s.state(ctx, prefix+req.Destination, &dest, func() error { var e error; dest, e = reader.Account(ctx, req.Destination); return e })
	if errors.Is(err, stellar.ErrPreflightAccountMissing) {
		return fail("destination_missing")
	}
	if err != nil {
		return nil, fmt.Errorf("simulation destination: %w", err)
	}
	if dest.AccountID != req.Destination {
		return nil, fmt.Errorf("invalid simulation destination state")
	}
	result.Cached = netHit && sourceHit && destHit
	if source.SubentryCount < 0 || source.NumSponsoring < 0 || source.NumSponsored < 0 {
		return nil, fmt.Errorf("invalid reserve counts")
	}
	// Decimal arithmetic avoids integer overflow and counts sponsored reserves.
	units := decimal.NewFromInt(2).Add(decimal.NewFromInt(source.SubentryCount)).Add(decimal.NewFromInt(source.NumSponsoring)).Sub(decimal.NewFromInt(source.NumSponsored))
	if units.IsNegative() {
		return nil, fmt.Errorf("invalid sponsored reserve count")
	}
	reserve := units.Mul(decimal.NewFromInt(net.BaseReserve)).Shift(-7)
	xlm := findSimulationBalance(source, "XLM", "")
	if xlm == nil {
		return nil, fmt.Errorf("source native balance missing")
	}
	spendable, err := simulationAvailable(*xlm)
	if err != nil {
		return nil, err
	}
	spendable = spendable.Sub(reserve)
	required := decimal.NewFromInt(net.Fee).Shift(-7)
	if native {
		required = required.Add(amount)
	}
	if spendable.LessThan(required) {
		return fail("insufficient_native_balance")
	}
	if !native && w.PublicKey != req.Issuer {
		balance := findSimulationBalance(source, req.Asset, req.Issuer)
		if balance == nil {
			return fail("source_trustline_missing")
		}
		if !balance.Authorized {
			return fail("source_trustline_unauthorized")
		}
		available, e := simulationAvailable(*balance)
		if e != nil {
			return nil, e
		}
		if available.LessThan(amount) {
			return fail("insufficient_asset_balance")
		}
	}
	if native || req.Destination != req.Issuer {
		balance := findSimulationBalance(dest, req.Asset, req.Issuer)
		if balance == nil {
			return fail("destination_trustline_missing")
		}
		if !native && !balance.Authorized {
			return fail("destination_trustline_unauthorized")
		}
		held, e := simulationAmount(balance.Balance)
		if e != nil {
			return nil, e
		}
		buying, e := simulationLiability(balance.Buying)
		if e != nil {
			return nil, e
		}
		limit := decimal.NewFromInt(math.MaxInt64).Shift(-7)
		if !native {
			limit, e = simulationAmount(balance.Limit)
			if e != nil {
				return nil, e
			}
		}
		if held.Add(buying).Add(amount).GreaterThan(limit) {
			return fail("destination_capacity_exceeded")
		}
	}
	result.Success = true
	return result, nil
}

func simulationAmount(raw string) (decimal.Decimal, error) {
	// Bound parsing work and require Stellar's plain decimal representation.
	if len(raw) == 0 || len(raw) > 32 {
		return decimal.Zero, fmt.Errorf("invalid stellar amount")
	}
	for _, c := range raw {
		if (c < '0' || c > '9') && c != '.' {
			return decimal.Zero, fmt.Errorf("invalid stellar amount")
		}
	}
	d, err := decimal.NewFromString(raw)
	if err != nil || d.IsNegative() || !d.Equal(d.Truncate(7)) || d.GreaterThan(decimal.NewFromInt(math.MaxInt64).Shift(-7)) {
		return decimal.Zero, fmt.Errorf("invalid stellar amount")
	}
	return d, nil
}
func simulationLiability(raw string) (decimal.Decimal, error) {
	if raw == "" {
		return decimal.Zero, nil
	}
	return simulationAmount(raw)
}
func simulationAvailable(b stellar.PreflightBalance) (decimal.Decimal, error) {
	held, err := simulationAmount(b.Balance)
	if err != nil {
		return decimal.Zero, err
	}
	selling, err := simulationLiability(b.Selling)
	if err != nil {
		return decimal.Zero, err
	}
	return held.Sub(selling), nil
}
func findSimulationBalance(a stellar.PreflightAccount, code, issuer string) *stellar.PreflightBalance {
	for i := range a.Balances {
		b := &a.Balances[i]
		if (code == "" || code == "XLM") && issuer == "" && b.Type == "native" {
			return b
		}
		if issuer != "" && b.Code == code && b.Issuer == issuer && (b.Type == "credit_alphanum4" || b.Type == "credit_alphanum12") {
			return b
		}
	}
	return nil
}
