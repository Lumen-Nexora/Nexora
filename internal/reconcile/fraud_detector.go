package reconcile

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/shopspring/decimal"
)

// FraudFindingKind distinguishes the two attack shapes the detector knows
// about. Both are surfaced to security review; skimming additionally freezes
// high-value sweeps before more money leaves the platform.
type FraudFindingKind string

const (
	FindingFrequencyAnomaly FraudFindingKind = "discrepancy_frequency_anomaly"
	FindingMicroVariance    FraudFindingKind = "micro_variance_skimming"
)

// FraudObservation is one discrepancy seen by reconciliation. TransferAmount
// is the value that moved; Discrepancy is the absolute difference between the
// expected and observed balance. RouteID identifies the corridor or wallet
// pair, letting the skimming detector aggregate micro-variances per route.
type FraudObservation struct {
	TenantID       string
	AccountID      string
	WalletID       string
	RouteID        string
	Asset          string
	TransferAmount decimal.Decimal
	Discrepancy    decimal.Decimal
	RouteVolume    int
	ObservedAt     time.Time
}

// AccountSecurityFlag records an automated action taken against an account.
type AccountSecurityFlag struct {
	TenantID  string    `json:"tenant_id"`
	AccountID string    `json:"account_id"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

const (
	SecurityActionReview = "security_review"
	SecurityActionFreeze = "freeze_high_value_sweeps"
)

// SecurityActionStore applies automated fraud containment. Implementations
// may persist to the database; the in-memory implementation is used when no
// durable store is configured and by tests.
type SecurityActionStore interface {
	FlagAccountForReview(ctx context.Context, tenantID, accountID, reason string) error
	FreezeHighValueSweeps(ctx context.Context, tenantID, accountID, reason string) error
}

// MemorySecurityActionStore is a concurrency-safe audit trail of automated
// actions. It is intentionally append-only.
type MemorySecurityActionStore struct {
	mu    sync.RWMutex
	flags []AccountSecurityFlag
	now   func() time.Time
}

func NewMemorySecurityActionStore() *MemorySecurityActionStore {
	return &MemorySecurityActionStore{now: time.Now}
}

func (m *MemorySecurityActionStore) FlagAccountForReview(_ context.Context, tenantID, accountID, reason string) error {
	m.append(tenantID, accountID, SecurityActionReview, reason)
	return nil
}

func (m *MemorySecurityActionStore) FreezeHighValueSweeps(_ context.Context, tenantID, accountID, reason string) error {
	m.append(tenantID, accountID, SecurityActionFreeze, reason)
	return nil
}

func (m *MemorySecurityActionStore) append(tenantID, accountID, action, reason string) {
	now := time.Now().UTC()
	if m.now != nil {
		now = m.now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flags = append(m.flags, AccountSecurityFlag{
		TenantID: tenantID, AccountID: accountID, Action: action, Reason: reason, CreatedAt: now,
	})
}

// Flags returns a copy of every action taken, newest last.
func (m *MemorySecurityActionStore) Flags() []AccountSecurityFlag {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AccountSecurityFlag, len(m.flags))
	copy(out, m.flags)
	return out
}

// FraudConfig tunes the detector. Zero values are replaced with conservative
// defaults by NewFraudDetector.
type FraudConfig struct {
	// Bucket is the aggregation granularity for discrepancy frequency.
	Bucket time.Duration
	// BaselineBuckets is how many completed buckets form the statistical
	// baseline for the per-account z-score.
	BaselineBuckets int
	// FrequencyZScore is the number of standard deviations above the baseline
	// mean that triggers a finding.
	FrequencyZScore float64
	// MinFrequencyEvents prevents a single discrepancy in a quiet account from
	// being treated as an anomaly.
	MinFrequencyEvents int

	// Window is the sliding window for route-level skimming detection.
	Window time.Duration
	// MicroVarianceMax is the largest per-transfer discrepancy that still looks
	// like skimming rather than an ordinary reconciliation error.
	MicroVarianceMax decimal.Decimal
	// MicroVarianceRatio is the maximum discrepancy as a fraction of transfer
	// amount. Skimming stays well below this; a wrong fee or FX rate does not.
	MicroVarianceRatio decimal.Decimal
	// MinSkimmingEvents is the minimum number of micro-variances on one route
	// before it is considered a pattern.
	MinSkimmingEvents int
	// HighVolumeTransfers is the route volume at which skimming becomes
	// economically meaningful.
	HighVolumeTransfers int
	// HighValueSweepAmount freezes sweeps whose aggregate observed amount is at
	// least this value.
	HighValueSweepAmount decimal.Decimal
}

func (c FraudConfig) withDefaults() FraudConfig {
	if c.Bucket <= 0 {
		c.Bucket = time.Minute
	}
	if c.BaselineBuckets <= 0 {
		c.BaselineBuckets = 5
	}
	if c.FrequencyZScore <= 0 {
		c.FrequencyZScore = 3
	}
	if c.MinFrequencyEvents <= 0 {
		c.MinFrequencyEvents = 3
	}
	if c.Window <= 0 {
		c.Window = 5 * time.Minute
	}
	if c.MicroVarianceMax.IsZero() {
		c.MicroVarianceMax = decimal.RequireFromString("0.01")
	}
	if c.MicroVarianceRatio.IsZero() {
		c.MicroVarianceRatio = decimal.RequireFromString("0.001")
	}
	if c.MinSkimmingEvents <= 0 {
		c.MinSkimmingEvents = 10
	}
	if c.HighVolumeTransfers <= 0 {
		c.HighVolumeTransfers = 20
	}
	if c.HighValueSweepAmount.IsZero() {
		c.HighValueSweepAmount = decimal.NewFromInt(1000)
	}
	return c
}

// FraudFinding is an anomaly that has been contained and paged out.
type FraudFinding struct {
	Kind         FraudFindingKind `json:"kind"`
	TenantID     string           `json:"tenant_id"`
	AccountID    string           `json:"account_id,omitempty"`
	WalletID     string           `json:"wallet_id,omitempty"`
	RouteID      string           `json:"route_id,omitempty"`
	Asset        string           `json:"asset,omitempty"`
	Score        float64          `json:"score"`
	Count        int              `json:"count"`
	TotalSkimmed decimal.Decimal  `json:"total_skimmed,omitempty"`
	Reason       string           `json:"reason"`
	DetectedAt   time.Time        `json:"detected_at"`
}

type frequencyWindow struct {
	bucket        time.Time
	current       int
	baseline      []int
	lastFlagged   time.Time
	lastFlagValue int
}

type microEvent struct {
	at          time.Time
	amount      decimal.Decimal
	discrepancy decimal.Decimal
}

type routeWindow struct {
	start        time.Time
	observations int
	events       []microEvent
	lastFlagged  time.Time
}

// FraudDetector maintains per-account discrepancy frequency statistics and
// per-route micro-variance accumulators. All state is guarded by a single
// mutex because observations arrive from concurrent reconciliation workers.
type FraudDetector struct {
	cfg     FraudConfig
	actions SecurityActionStore
	alerter alerting.SecurityAlerter
	now     func() time.Time

	mu       sync.Mutex
	accounts map[string]*frequencyWindow
	routes   map[string]*routeWindow
}

func NewFraudDetector(cfg FraudConfig, actions SecurityActionStore, alerter alerting.SecurityAlerter) *FraudDetector {
	if actions == nil {
		actions = NewMemorySecurityActionStore()
	}
	return &FraudDetector{
		cfg:      cfg.withDefaults(),
		actions:  actions,
		alerter:  alerter,
		now:      time.Now,
		accounts: make(map[string]*frequencyWindow),
		routes:   make(map[string]*routeWindow),
	}
}

// WithClock replaces the time source. It is intended for deterministic tests.
func (d *FraudDetector) WithClock(now func() time.Time) *FraudDetector {
	if now != nil {
		d.now = now
	}
	return d
}

// Config exposes the resolved configuration.
func (d *FraudDetector) Config() FraudConfig {
	return d.cfg
}

// Observe evaluates one discrepancy and returns any findings it produced. The
// detector applies containment (flag/freeze) and dispatches PagerDuty alerts
// before returning; action and alert failures are joined into the error while
// the findings are still returned.
func (d *FraudDetector) Observe(ctx context.Context, obs FraudObservation) ([]FraudFinding, error) {
	if d == nil {
		return nil, nil
	}
	now := obs.ObservedAt
	if now.IsZero() {
		now = d.now()
	}
	now = now.UTC()
	tenantID := nonEmpty(obs.TenantID, "unknown")
	accountID := nonEmpty(obs.AccountID, obs.WalletID)
	if accountID == "" {
		accountID = "unknown"
	}

	d.mu.Lock()
	findings := d.observeFrequencyLocked(tenantID, accountID, obs, now)
	findings = append(findings, d.observeSkimmingLocked(tenantID, accountID, obs, now)...)
	d.mu.Unlock()

	var errs []error
	for i := range findings {
		finding := findings[i]
		if err := d.actions.FlagAccountForReview(ctx, finding.TenantID, finding.AccountID, finding.Reason); err != nil {
			errs = append(errs, fmt.Errorf("flag account %s for review: %w", finding.AccountID, err))
		}
		if finding.Kind == FindingMicroVariance && finding.TotalSkimmed.GreaterThanOrEqual(d.cfg.HighValueSweepAmount) {
			if err := d.actions.FreezeHighValueSweeps(ctx, finding.TenantID, finding.AccountID, finding.Reason); err != nil {
				errs = append(errs, fmt.Errorf("freeze high-value sweeps for %s: %w", finding.AccountID, err))
			}
		}
		if d.alerter != nil {
			if err := d.alerter.Send(ctx, finding.alert()); err != nil {
				errs = append(errs, fmt.Errorf("dispatch PagerDuty alert: %w", err))
			}
		}
	}

	return findings, errors.Join(errs...)
}

func (d *FraudDetector) observeFrequencyLocked(tenantID, accountID string, obs FraudObservation, now time.Time) []FraudFinding {
	key := tenantID + "|" + accountID
	window := d.accounts[key]
	if window == nil {
		window = &frequencyWindow{bucket: now.Truncate(d.cfg.Bucket)}
		d.accounts[key] = window
	}

	bucket := now.Truncate(d.cfg.Bucket)
	if bucket.After(window.bucket) {
		window.baseline = append(window.baseline, window.current)
		if len(window.baseline) > d.cfg.BaselineBuckets {
			window.baseline = window.baseline[len(window.baseline)-d.cfg.BaselineBuckets:]
		}
		// A gap in observations means the account was quiet; record zeros so a
		// burst after a lull is compared against the lull, not against itself.
		for b := window.bucket.Add(d.cfg.Bucket); b.Before(bucket); b = b.Add(d.cfg.Bucket) {
			window.baseline = append(window.baseline, 0)
			if len(window.baseline) > d.cfg.BaselineBuckets {
				window.baseline = window.baseline[len(window.baseline)-d.cfg.BaselineBuckets:]
			}
		}
		window.bucket = bucket
		window.current = 0
	}
	window.current++

	if window.lastFlagged.Equal(bucket) {
		return nil
	}
	if len(window.baseline) < d.cfg.BaselineBuckets || window.current < d.cfg.MinFrequencyEvents {
		return nil
	}

	mean, stddev := meanStdDev(window.baseline)
	if stddev < 1 {
		stddev = 1
	}
	z := (float64(window.current) - mean) / stddev
	if z < d.cfg.FrequencyZScore {
		return nil
	}

	window.lastFlagged = bucket
	window.lastFlagValue = window.current
	return []FraudFinding{{
		Kind:       FindingFrequencyAnomaly,
		TenantID:   tenantID,
		AccountID:  accountID,
		WalletID:   obs.WalletID,
		Asset:      obs.Asset,
		Score:      z,
		Count:      window.current,
		Reason:     fmt.Sprintf("discrepancy frequency %d in %s is %.1f standard deviations above baseline", window.current, d.cfg.Bucket, z),
		DetectedAt: now,
	}}
}

func (d *FraudDetector) observeSkimmingLocked(tenantID, accountID string, obs FraudObservation, now time.Time) []FraudFinding {
	routeID := nonEmpty(obs.RouteID, obs.WalletID)
	if routeID == "" {
		routeID = accountID
	}
	key := tenantID + "|" + routeID
	window := d.routes[key]
	if window == nil || now.Sub(window.start) > d.cfg.Window {
		window = &routeWindow{start: now}
		d.routes[key] = window
	}
	window.observations++
	if obs.RouteVolume > window.observations {
		window.observations = obs.RouteVolume
	}

	discrepancy := obs.Discrepancy.Abs()
	if discrepancy.IsZero() || discrepancy.GreaterThan(d.cfg.MicroVarianceMax) {
		return nil
	}
	if obs.TransferAmount.IsNegative() || obs.TransferAmount.IsZero() {
		return nil
	}
	ratio := discrepancy.Div(obs.TransferAmount)
	if ratio.GreaterThan(d.cfg.MicroVarianceRatio) {
		return nil
	}

	window.events = append(window.events, microEvent{
		at: now, amount: obs.TransferAmount, discrepancy: discrepancy,
	})

	// Drop events that have aged out of the sliding window.
	cutoff := now.Add(-d.cfg.Window)
	kept := window.events[:0]
	for _, event := range window.events {
		if !event.at.Before(cutoff) {
			kept = append(kept, event)
		}
	}
	window.events = kept

	if len(window.events) < d.cfg.MinSkimmingEvents {
		return nil
	}
	if window.observations < d.cfg.HighVolumeTransfers {
		return nil
	}
	if !window.lastFlagged.IsZero() && now.Sub(window.lastFlagged) < d.cfg.Window {
		return nil
	}

	totalSkimmed := decimal.Zero
	totalAmount := decimal.Zero
	for _, event := range window.events {
		totalSkimmed = totalSkimmed.Add(event.discrepancy)
		totalAmount = totalAmount.Add(event.amount)
	}

	window.lastFlagged = now
	score := float64(len(window.events))
	return []FraudFinding{{
		Kind:         FindingMicroVariance,
		TenantID:     tenantID,
		AccountID:    accountID,
		WalletID:     obs.WalletID,
		RouteID:      routeID,
		Asset:        obs.Asset,
		Score:        score,
		Count:        len(window.events),
		TotalSkimmed: totalSkimmed,
		Reason: fmt.Sprintf("micro-variance skimming: %d sub-threshold discrepancies totalling %s across %d transfers on route %s",
			len(window.events), totalSkimmed.String(), window.observations, routeID),
		DetectedAt: now,
	}}
}

func (f FraudFinding) alert() alerting.SecurityAlert {
	severity := alerting.SeverityError
	if f.Kind == FindingMicroVariance {
		severity = alerting.SeverityCritical
	}
	details := map[string]interface{}{
		"kind":          string(f.Kind),
		"tenant_id":     f.TenantID,
		"account_id":    f.AccountID,
		"wallet_id":     f.WalletID,
		"route_id":      f.RouteID,
		"asset":         f.Asset,
		"score":         f.Score,
		"count":         f.Count,
		"total_skimmed": f.TotalSkimmed.String(),
	}
	return alerting.SecurityAlert{
		Summary:   "Nexora fraud alert: " + string(f.Kind),
		Source:    "nexora-reconcile",
		Severity:  severity,
		DedupKey:  fmt.Sprintf("fraud:%s:%s:%s:%d", f.Kind, f.TenantID, nonEmpty(f.RouteID, f.AccountID), f.DetectedAt.Truncate(time.Minute).Unix()),
		Details:   details,
		Timestamp: f.DetectedAt,
	}
}

func meanStdDev(values []int) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, value := range values {
		sum += float64(value)
	}
	mean := sum / float64(len(values))
	variance := 0.0
	for _, value := range values {
		delta := float64(value) - mean
		variance += delta * delta
	}
	variance /= float64(len(values))
	return mean, math.Sqrt(variance)
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
