package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

const (
	// InvariantHaltSource identifies the sweep halt owned by the invariant
	// checker. The checker only ever clears its own halt.
	InvariantHaltSource = "ledger_invariant"
	// defaultInvariantTimeout bounds one invariant evaluation so a slow
	// aggregate can never pile up behind the next 60-second tick.
	defaultInvariantTimeout = 15 * time.Second
	// maxInvariantAlertAssets caps how many assets are spelled out in one
	// alert so a broad corruption cannot produce an unbounded message.
	maxInvariantAlertAssets = 20
	// maxHaltReasonLen bounds the reason stored on the sweep halt row.
	maxHaltReasonLen = 1000
)

// InvariantTotals are the per-asset aggregates read from one consistent
// database snapshot. All amounts are exact decimals.
type InvariantTotals struct {
	Asset           string
	TenantBalances  decimal.Decimal
	AccumulatedFees decimal.Decimal
	PooledReserves  decimal.Decimal
}

// InvariantResult is the evaluation of the zero-sum invariant for one asset:
//
//	TenantBalances + AccumulatedFees == PooledReserves
//
// Difference is PooledReserves - (TenantBalances + AccumulatedFees); a
// positive value means the reserves hold unexplained funds, a negative one
// means tenant claims exceed what the platform holds.
type InvariantResult struct {
	Asset           string          `json:"asset"`
	TenantBalances  decimal.Decimal `json:"tenant_balances"`
	AccumulatedFees decimal.Decimal `json:"accumulated_fees"`
	PooledReserves  decimal.Decimal `json:"pooled_reserves"`
	Expected        decimal.Decimal `json:"expected"`
	Difference      decimal.Decimal `json:"difference"`
	Balanced        bool            `json:"balanced"`
}

// InvariantReport summarises one run of the checker.
type InvariantReport struct {
	CheckedAt time.Time `json:"checked_at"`
	// Configured is false when no pooled reserve wallets are registered, in
	// which case nothing was evaluated and no sweep was halted.
	Configured bool              `json:"configured"`
	Assets     []InvariantResult `json:"assets"`
	Violations []InvariantResult `json:"violations"`
	// SweepsHalted reports whether automated sweeps are halted by the
	// checker after this run.
	SweepsHalted bool `json:"sweeps_halted"`
}

// Balanced reports whether every evaluated asset satisfied the invariant.
func (r *InvariantReport) Balanced() bool { return len(r.Violations) == 0 }

// InvariantRepository reads the invariant aggregates. Implementations must
// read every total from a single read-only snapshot so concurrent balance
// writes can never produce a torn (and falsely failing) comparison.
type InvariantRepository interface {
	// GetInvariantTotals returns one row per asset. configured is false when
	// no pooled reserve wallet is registered.
	GetInvariantTotals(ctx context.Context) (totals []InvariantTotals, configured bool, err error)
}

// SweepHalter persists the halt that the automated sweep workers consult.
type SweepHalter interface {
	// HaltSweeps records a halt for source. It is idempotent and reports
	// whether the halt is new, so callers page once per incident.
	HaltSweeps(ctx context.Context, source, reason string) (newlyHalted bool, err error)
	// ResumeSweeps removes the halt recorded by source only. It reports
	// whether a halt was actually removed.
	ResumeSweeps(ctx context.Context, source string) (resumed bool, err error)
}

// EvaluateInvariant applies the zero-sum invariant to each asset. It is pure
// and uses exact decimal arithmetic, so any non-zero difference — however
// small or large — is a violation. Results are sorted by asset.
func EvaluateInvariant(totals []InvariantTotals) []InvariantResult {
	results := make([]InvariantResult, 0, len(totals))
	for _, t := range totals {
		expected := t.TenantBalances.Add(t.AccumulatedFees)
		diff := t.PooledReserves.Sub(expected)
		results = append(results, InvariantResult{
			Asset:           t.Asset,
			TenantBalances:  t.TenantBalances,
			AccumulatedFees: t.AccumulatedFees,
			PooledReserves:  t.PooledReserves,
			Expected:        expected,
			Difference:      diff,
			Balanced:        diff.IsZero(),
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Asset < results[j].Asset })
	return results
}

// InvariantChecker verifies the global zero-sum invariant across tenant
// balances, accumulated fees and pooled reserves. On a violation it pages
// through the alerting client and halts automated sweeps; once the books
// balance again it lifts its own halt. Running it repeatedly is safe: the
// halt is idempotent and an ongoing incident pages only once.
type InvariantChecker struct {
	repo     InvariantRepository
	halter   SweepHalter
	alerting *alerting.Client
	timeout  time.Duration

	// mu serialises runs within one process; the halt row makes the
	// outcome idempotent across processes.
	mu sync.Mutex
	// violated is the in-process incident state, used to page once when no
	// halter is wired or its write fails.
	violated bool
}

// NewInvariantChecker builds a checker. halter and alertClient may be nil:
// without a halter the sweeps cannot be halted (violations are still
// paged), and without an alert client violations are only logged.
func NewInvariantChecker(repo InvariantRepository, halter SweepHalter, alertClient *alerting.Client) *InvariantChecker {
	return &InvariantChecker{
		repo:     repo,
		halter:   halter,
		alerting: alertClient,
		timeout:  defaultInvariantTimeout,
	}
}

// WithTimeout overrides the per-run deadline. Non-positive values are ignored.
func (c *InvariantChecker) WithTimeout(d time.Duration) *InvariantChecker {
	if d > 0 {
		c.timeout = d
	}
	return c
}

// Check runs one invariant evaluation. A read failure is returned as an
// error and never treated as a violation, so an unavailable database cannot
// halt sweeps on its own.
func (c *InvariantChecker) Check(ctx context.Context) (*InvariantReport, error) {
	if c.repo == nil {
		return nil, errors.New("invariant checker: repository is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	report := &InvariantReport{CheckedAt: time.Now().UTC()}
	totals, configured, err := c.repo.GetInvariantTotals(ctx)
	if err != nil {
		return nil, fmt.Errorf("read invariant totals: %w", err)
	}
	report.Configured = configured
	if !configured {
		log.Debug().Msg("reconcile: invariant check skipped, no pooled reserve wallets registered")
		return report, nil
	}

	report.Assets = EvaluateInvariant(totals)
	for _, r := range report.Assets {
		if !r.Balanced {
			report.Violations = append(report.Violations, r)
		}
	}

	if report.Balanced() {
		c.restore(ctx, report)
		return report, nil
	}
	c.violate(ctx, report)
	return report, nil
}

func (c *InvariantChecker) violate(ctx context.Context, report *InvariantReport) {
	summary := formatInvariantViolations(report.Violations)
	for _, v := range report.Violations {
		log.Error().
			Str("asset", v.Asset).
			Str("tenant_balances", v.TenantBalances.String()).
			Str("accumulated_fees", v.AccumulatedFees.String()).
			Str("pooled_reserves", v.PooledReserves.String()).
			Str("difference", v.Difference.String()).
			Msg("reconcile: zero-sum ledger invariant violated")
	}

	page := !c.violated
	if c.halter != nil {
		newly, err := c.halter.HaltSweeps(ctx, InvariantHaltSource, truncateHaltReason(summary))
		if err != nil {
			// The halt could not be recorded: page on every run until it
			// can, because sweeps may still be running.
			log.Error().Err(err).Msg("reconcile: failed to halt automated sweeps")
			page = true
		} else {
			report.SweepsHalted = true
			page = page || newly
		}
	}
	c.violated = true

	if page && c.alerting != nil {
		action := "Automated sweeps have been halted until the books balance."
		if !report.SweepsHalted {
			action = "WARNING: automated sweeps could NOT be halted."
		}
		c.alerting.Critical(ctx, "Ledger Zero-Sum Invariant Violated",
			fmt.Sprintf("Sum(TenantBalances) + AccumulatedFees != PooledReserveBalances for %d asset(s). %s\n%s",
				len(report.Violations), action, summary))
	}
}

func (c *InvariantChecker) restore(ctx context.Context, report *InvariantReport) {
	resumed := false
	if c.halter != nil {
		var err error
		resumed, err = c.halter.ResumeSweeps(ctx, InvariantHaltSource)
		if err != nil {
			// Keep the incident open so the next run retries the resume.
			log.Error().Err(err).Msg("reconcile: failed to lift invariant sweep halt")
			report.SweepsHalted = true
			return
		}
	}
	wasViolated := c.violated
	c.violated = false
	if (resumed || wasViolated) && c.alerting != nil {
		c.alerting.Info(ctx, "Ledger Zero-Sum Invariant Restored",
			fmt.Sprintf("All %d asset(s) balance again; automated sweeps resumed.", len(report.Assets)))
	}
}

func formatInvariantViolations(violations []InvariantResult) string {
	var b strings.Builder
	for i, v := range violations {
		if i == maxInvariantAlertAssets {
			fmt.Fprintf(&b, "... and %d more asset(s)\n", len(violations)-i)
			break
		}
		fmt.Fprintf(&b, "%s: tenants=%s fees=%s pooled=%s difference=%s\n",
			v.Asset, v.TenantBalances.String(), v.AccumulatedFees.String(),
			v.PooledReserves.String(), v.Difference.String())
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateHaltReason keeps the persisted halt reason bounded.
func truncateHaltReason(s string) string {
	if len(s) <= maxHaltReasonLen {
		return s
	}
	return s[:maxHaltReasonLen]
}
