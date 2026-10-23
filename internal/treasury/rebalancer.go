package treasury

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// RunwayHours is the operational runway the warm wallet must hold at all times.
const RunwayHours = 48

// RebalanceStatus describes the outcome of a single rebalance evaluation.
type RebalanceStatus string

const (
	RebalanceStatusHealthy   RebalanceStatus = "healthy"
	RebalanceStatusReplenish RebalanceStatus = "replenish_required"
	RebalanceStatusEmergency RebalanceStatus = "emergency"
)

// WarmWalletPolicy configures a single warm/hot operational wallet.
type WarmWalletPolicy struct {
	Asset string
	// EmergencyThreshold is the absolute balance below which the treasury team
	// is paged immediately, regardless of burn rate.
	EmergencyThreshold decimal.Decimal
}

// RebalanceRequest is a multi-sig replenishment request for cold storage signers.
// It carries an unsigned Stellar payment envelope, so signers review the exact
// operation before approving.
type RebalanceRequest struct {
	Asset          string          `json:"asset"`
	Amount         decimal.Decimal `json:"amount"`
	Source         string          `json:"source_cold_wallet"`
	Destination    string          `json:"destination_warm_wallet"`
	RequiredSigs   int             `json:"required_signatures"`
	UnsignedXDR    string          `json:"unsigned_xdr"`
	CreatedAt      time.Time       `json:"created_at"`
	BurnRatePerDay decimal.Decimal `json:"burn_rate_per_day"`
	RunwayTarget   decimal.Decimal `json:"runway_target"`
}

// RebalanceEvent is one historical telemetry record.
type RebalanceEvent struct {
	Asset        string          `json:"asset"`
	Status       RebalanceStatus `json:"status"`
	Balance      decimal.Decimal `json:"balance"`
	BurnRate     decimal.Decimal `json:"burn_rate_per_day"`
	ReplenishAmt decimal.Decimal `json:"replenish_amount"`
	ObservedAt   time.Time       `json:"observed_at"`
}

// RebalanceTelemetry aggregates rebalancing frequency and treasury velocity.
type RebalanceTelemetry struct {
	Asset              string          `json:"asset"`
	Replenishments     int             `json:"replenishments"`
	Emergencies        int             `json:"emergencies"`
	TotalReplenished   decimal.Decimal `json:"total_replenished"`
	Window             time.Duration   `json:"window"`
	VelocityPerDay     decimal.Decimal `json:"velocity_per_day"`
	ReplenishPerDayAvg decimal.Decimal `json:"replenish_per_day_avg"`
}

// BurnRateSource supplies the rolling daily outflow of a warm wallet.
type BurnRateSource interface {
	DailyBurnRate(ctx context.Context, asset string) (decimal.Decimal, error)
}

// ColdStorageRequester persists and dispatches replenishment requests to the
// cold storage multi-sig signer workflow.
type ColdStorageRequester interface {
	SubmitReplenishment(ctx context.Context, req RebalanceRequest) error
}

// Rebalancer monitors warm wallet balances against burn rates.
type Rebalancer struct {
	stellar     stellar.Client
	warmWallet  string
	coldWallet  string
	signers     int
	policies    map[string]WarmWalletPolicy
	burn        BurnRateSource
	cold        ColdStorageRequester
	monitor     *alerting.Monitor
	now         func() time.Time

	mu        sync.Mutex
	history   []RebalanceEvent
}

// RebalancerConfig groups constructor inputs.
type RebalancerConfig struct {
	WarmWallet     string
	ColdWallet     string
	RequiredSigs   int
	Policies       []WarmWalletPolicy
	BurnRates      BurnRateSource
	ColdStorage    ColdStorageRequester
	Monitor        *alerting.Monitor
}

// NewRebalancer constructs a rebalancer. Returns an error on invalid config.
func NewRebalancer(client stellar.Client, cfg RebalancerConfig) (*Rebalancer, error) {
	if cfg.WarmWallet == "" || cfg.ColdWallet == "" {
		return nil, fmt.Errorf("rebalancer: warm and cold wallet addresses are required")
	}
	if cfg.RequiredSigs < 1 {
		return nil, fmt.Errorf("rebalancer: required signatures must be >= 1")
	}
	if cfg.BurnRates == nil || cfg.ColdStorage == nil {
		return nil, fmt.Errorf("rebalancer: burn rate source and cold storage requester are required")
	}
	policies := make(map[string]WarmWalletPolicy, len(cfg.Policies))
	for _, p := range cfg.Policies {
		if p.Asset == "" {
			return nil, fmt.Errorf("rebalancer: policy asset is required")
		}
		policies[p.Asset] = p
	}
	return &Rebalancer{
		stellar:    client,
		warmWallet: cfg.WarmWallet,
		coldWallet: cfg.ColdWallet,
		signers:    cfg.RequiredSigs,
		policies:   policies,
		burn:       cfg.BurnRates,
		cold:       cfg.ColdStorage,
		monitor:    cfg.Monitor,
		now:        time.Now,
	}, nil
}

// requiredRunway returns the amount needed to sustain RunwayHours of burn.
func requiredRunway(burnPerDay decimal.Decimal) decimal.Decimal {
	return burnPerDay.Mul(decimal.NewFromInt(RunwayHours)).Div(decimal.NewFromInt(24))
}

// Evaluate checks one asset's warm balance and returns the status and the
// replenishment amount (zero when healthy).
func (r *Rebalancer) Evaluate(ctx context.Context, asset string) (RebalanceStatus, decimal.Decimal, error) {
	policy, ok := r.policies[asset]
	if !ok {
		return "", decimal.Zero, fmt.Errorf("rebalancer: no policy for asset %s", asset)
	}

	balance, err := r.warmBalance(ctx, asset)
	if err != nil {
		return "", decimal.Zero, err
	}
	burn, err := r.burn.DailyBurnRate(ctx, asset)
	if err != nil {
		return "", decimal.Zero, fmt.Errorf("rebalancer: burn rate %s: %w", asset, err)
	}

	runway := requiredRunway(burn)
	status := RebalanceStatusHealthy
	replenish := decimal.Zero

	if balance.LessThan(runway) {
		replenish = runway.Sub(balance)
		status = RebalanceStatusReplenish
	}
	if balance.LessThan(policy.EmergencyThreshold) {
		status = RebalanceStatusEmergency
		if replenish.IsZero() {
			replenish = runway.Sub(balance).Max(decimal.Zero)
		}
	}

	r.record(RebalanceEvent{
		Asset:        asset,
		Status:       status,
		Balance:      balance,
		BurnRate:     burn,
		ReplenishAmt: replenish,
		ObservedAt:   r.now().UTC(),
	})

	if err := r.alert(ctx, asset, status, balance, policy.EmergencyThreshold, runway); err != nil {
		log.Error().Err(err).Str("asset", asset).Msg("rebalancer: alert delivery failed")
	}

	return status, replenish, nil
}

// Run evaluates every configured asset and, when replenishment is required,
// generates and submits a multi-sig request to cold storage.
func (r *Rebalancer) Run(ctx context.Context) error {
	var errs []error
	for asset := range r.policies {
		status, amount, err := r.Evaluate(ctx, asset)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if status == RebalanceStatusHealthy || amount.IsZero() {
			continue
		}

		burn, err := r.burn.DailyBurnRate(ctx, asset)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		xdr, err := r.buildReplenishmentXDR(ctx, asset, amount)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		req := RebalanceRequest{
			Asset:          asset,
			Amount:         amount,
			Source:         r.coldWallet,
			Destination:    r.warmWallet,
			RequiredSigs:   r.signers,
			UnsignedXDR:    xdr,
			CreatedAt:      r.now().UTC(),
			BurnRatePerDay: burn,
			RunwayTarget:   requiredRunway(burn),
		}
		if err := r.cold.SubmitReplenishment(ctx, req); err != nil {
			errs = append(errs, fmt.Errorf("rebalancer: submit replenishment %s: %w", asset, err))
		}
	}
	return joinErrors(errs)
}

// Telemetry summarises rebalancing frequency and velocity for an asset over
// the given window.
func (r *Rebalancer) Telemetry(asset string, window time.Duration) RebalanceTelemetry {
	cutoff := r.now().Add(-window)
	t := RebalanceTelemetry{Asset: asset, Window: window}
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, ev := range r.history {
		if ev.Asset != asset || ev.ObservedAt.Before(cutoff) {
			continue
		}
		switch ev.Status {
		case RebalanceStatusReplenish:
			t.Replenishments++
			t.TotalReplenished = t.TotalReplenished.Add(ev.ReplenishAmt)
		case RebalanceStatusEmergency:
			t.Emergencies++
			t.TotalReplenished = t.TotalReplenished.Add(ev.ReplenishAmt)
		}
	}

	days := decimal.NewFromFloat(window.Hours() / 24)
	if days.IsPositive() {
		t.VelocityPerDay = t.TotalReplenished.Div(days)
		t.ReplenishPerDayAvg = decimal.NewFromInt(int64(t.Replenishments + t.Emergencies)).Div(days)
	}
	return t
}

// History returns a copy of recorded telemetry events.
func (r *Rebalancer) History() []RebalanceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RebalanceEvent, len(r.history))
	copy(out, r.history)
	return out
}

func (r *Rebalancer) record(ev RebalanceEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history = append(r.history, ev)
}

func (r *Rebalancer) warmBalance(ctx context.Context, asset string) (decimal.Decimal, error) {
	acct, err := stellar.LoadAccountWithContext(ctx, r.stellar, r.warmWallet)
	if err != nil {
		return decimal.Zero, fmt.Errorf("rebalancer: load warm wallet: %w", err)
	}
	for _, b := range acct.Balances {
		code := b.Code
		if code == "" {
			code = "XLM"
		}
		if code != asset {
			continue
		}
		amt, err := decimal.NewFromString(b.Balance)
		if err != nil {
			return decimal.Zero, fmt.Errorf("rebalancer: parse balance %s: %w", asset, err)
		}
		return amt, nil
	}
	return decimal.Zero, nil
}

func (r *Rebalancer) alert(ctx context.Context, asset string, status RebalanceStatus, balance, threshold, runway decimal.Decimal) error {
	if r.monitor == nil {
		return nil
	}
	key := "treasury-warm-emergency-" + asset
	inc := alerting.Incident{
		DedupKey: key,
		Source:   "treasury.rebalancer",
		Timestamp: r.now().UTC(),
		Details: map[string]any{
			"asset":               asset,
			"warm_balance":        balance.String(),
			"emergency_threshold": threshold.String(),
			"runway_target":       runway.String(),
		},
	}
	if status == RebalanceStatusEmergency {
		inc.Severity = alerting.SeverityCritical
		inc.Summary = fmt.Sprintf("Warm wallet %s balance %s below emergency threshold %s", asset, balance, threshold)
		return r.monitor.Raise(ctx, inc)
	}
	return r.monitor.Clear(ctx, alerting.Incident{DedupKey: key, Summary: "Warm wallet " + asset + " recovered", Source: inc.Source, Details: inc.Details})
}

// buildReplenishmentXDR builds an unsigned Stellar payment from the cold wallet
// to the warm wallet. Signers collect signatures out-of-band.
func (r *Rebalancer) buildReplenishmentXDR(ctx context.Context, asset string, amount decimal.Decimal) (string, error) {
	acct, err := stellar.LoadAccountWithContext(ctx, r.stellar, r.coldWallet)
	if err != nil {
		return "", fmt.Errorf("rebalancer: load cold wallet: %w", err)
	}
	return stellar.BuildUnsignedPaymentXDR(acct, r.warmWallet, asset, amount.StringFixed(7))
}

func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	msg := "rebalancer: "
	for i, e := range errs {
		if i > 0 {
			msg += "; "
		}
		msg += e.Error()
	}
	return fmt.Errorf("%s", msg)
}
