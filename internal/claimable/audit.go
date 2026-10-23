package claimable

import (
	"context"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/rs/zerolog/log"
	"github.com/stellar/go/protocols/horizon"
)

// AlertWindow is how far ahead of expiry a tenant is warned about a
// claimable balance that is still unclaimed.
const AlertWindow = 48 * time.Hour

// AuditFinding describes one claimable balance observed during an audit pass.
type AuditFinding struct {
	BalanceID string
	TenantID  *string
	Expired   bool
	ExpiresIn time.Duration
	NearExpiry bool
	Reclaimed bool
	Err       error
}

// AuditReport summarises one audit pass.
type AuditReport struct {
	Scanned     int
	Expired     int
	NearExpiry  int
	Reclaimed   int
	Failed      int
	Findings    []AuditFinding
}

// HorizonClaimableLister queries Horizon for claimable balances sponsored by
// a given account. Satisfied by stellar.ClaimableBalanceClient implementations
// that expose listing.
type HorizonClaimableLister interface {
	ClaimableBalancesBySponsor(sponsor string) ([]horizon.ClaimableBalance, error)
}

// ClaimableAlerter sends tenant-facing alerts. Dispatch is reused from the
// webhook dispatcher.
type ClaimableAlerter interface {
	Dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) error
}

// Auditor tracks Nexora-created claimable balances, detects expired claim
// predicates, reclaims expired funds to the tenant treasury, and alerts
// tenants on balances nearing expiry.
type Auditor struct {
	svc      *service
	lister   HorizonClaimableLister
	alerter  ClaimableAlerter
	sponsors []string
	now      func() time.Time
}

// NewAuditor constructs an Auditor over an existing claimable service. The
// sponsors slice lists Nexora-custodied account IDs whose sponsored balances
// are audited.
func NewAuditor(svc Service, lister HorizonClaimableLister, alerter ClaimableAlerter, sponsors []string) (*Auditor, error) {
	concrete, ok := svc.(*service)
	if !ok {
		return nil, fmt.Errorf("claimable auditor requires the default service implementation")
	}
	return &Auditor{
		svc:      concrete,
		lister:   lister,
		alerter:  alerter,
		sponsors: sponsors,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// Audit performs one full pass: it reads all active claimable balances
// sponsored by Nexora accounts from Horizon, evaluates each balance's claimant
// predicates against the current time, reclaims expired balances that are
// configured for revocation, and alerts tenants on balances within AlertWindow
// of expiry.
func (a *Auditor) Audit(ctx context.Context) (*AuditReport, error) {
	now := a.now()
	report := &AuditReport{}

	seen := make(map[string]struct{})
	for _, sponsor := range a.sponsors {
		balances, err := a.lister.ClaimableBalancesBySponsor(sponsor)
		if err != nil {
			return report, fmt.Errorf("list claimable balances for sponsor %s: %w", sponsor, err)
		}
		for _, hb := range balances {
			if _, dup := seen[hb.BalanceID]; dup {
				continue
			}
			seen[hb.BalanceID] = struct{}{}
			report.Scanned++

			finding := a.evaluate(ctx, hb, now)
			if finding.Expired {
				report.Expired++
			}
			if finding.NearExpiry {
				report.NearExpiry++
			}
			if finding.Reclaimed {
				report.Reclaimed++
			}
			if finding.Err != nil {
				report.Failed++
			}
			report.Findings = append(report.Findings, finding)
		}
	}

	return report, nil
}

func (a *Auditor) evaluate(ctx context.Context, hb horizon.ClaimableBalance, now time.Time) AuditFinding {
	finding := AuditFinding{BalanceID: hb.BalanceID}

	local, err := a.svc.repo.GetByID(ctx, hb.BalanceID)
	if err != nil {
		finding.Err = fmt.Errorf("load local record: %w", err)
		return finding
	}
	finding.TenantID = local.TenantID

	if local.Status != domain.ClaimableBalanceStatusPending {
		return finding
	}

	expiry, hasExpiry := ExpiryFromPredicates(local.Claimants, local.CreatedAt)
	if local.ExpiresAt != nil {
		expiry, hasExpiry = *local.ExpiresAt, true
	}

	if hasExpiry {
		finding.ExpiresIn = expiry.Sub(now)
	}

	if hasExpiry && !now.Before(expiry) {
		finding.Expired = true
		if local.RevokeOnExpiry {
			if err := a.reclaim(ctx, local, now); err != nil {
				finding.Err = err
			} else {
				finding.Reclaimed = true
			}
		}
		return finding
	}

	if hasExpiry && expiry.Sub(now) <= AlertWindow {
		finding.NearExpiry = true
		a.alertNearExpiry(ctx, local, expiry)
	}

	return finding
}

// reclaim executes a ClaimClaimableBalance operation for an expired balance,
// routing the funds to a custodied claimant that is still eligible under its
// predicate (the tenant treasury), and marks the local record accordingly.
func (a *Auditor) reclaim(ctx context.Context, balance *domain.ClaimableBalance, now time.Time) error {
	var treasury *domain.Claimant
	for i := range balance.Claimants {
		c := &balance.Claimants[i]
		if !Satisfiable(c.Predicate, now, balance.CreatedAt) {
			continue
		}
		if w, err := a.svc.wallets.GetByPublicKey(ctx, c.Account); err == nil && w != nil {
			treasury = c
			break
		}
	}
	if treasury == nil {
		return fmt.Errorf("%w: no custodied claimant can reclaim %s", domain.ErrClaimantNotCustodied, balance.ID)
	}

	wallet, err := a.svc.wallets.GetByPublicKey(ctx, treasury.Account)
	if err != nil || wallet == nil {
		return fmt.Errorf("%w: %s", domain.ErrClaimantNotCustodied, treasury.Account)
	}

	if err := a.svc.repo.MarkClaimed(ctx, balance.ID, treasury.Account, now); err != nil {
		return fmt.Errorf("mark reclaimed %s: %w", balance.ID, err)
	}

	txHash, err := a.svc.submitClaim(ctx, balance.ID, wallet)
	if err != nil {
		_ = a.svc.repo.MarkExpired(ctx, balance.ID, now)
		return fmt.Errorf("reclaim %s: %w", balance.ID, err)
	}

	a.svc.dispatch(ctx, domain.EventClaimableBalanceClaimed, map[string]interface{}{
		"balance_id": balance.ID,
		"asset":      balance.Asset,
		"amount":     balance.Amount.StringFixed(7),
		"claimed_by": treasury.Account,
		"reclaimed":  true,
		"tx_hash":    txHash,
	})

	log.Info().Str("balance_id", balance.ID).Str("treasury", treasury.Account).
		Str("tx_hash", txHash).Msg("claimable: expired balance reclaimed to treasury")
	return nil
}

func (a *Auditor) alertNearExpiry(ctx context.Context, balance *domain.ClaimableBalance, expiry time.Time) {
	if a.alerter == nil {
		return
	}
	if err := a.alerter.Dispatch(ctx, domain.EventClaimableBalanceExpiring, map[string]interface{}{
		"balance_id": balance.ID,
		"asset":      balance.Asset,
		"amount":     balance.Amount.StringFixed(7),
		"expires_at": expiry.UTC().Format(time.RFC3339),
		"window_hrs": int(AlertWindow.Hours()),
	}); err != nil {
		log.Warn().Err(err).Str("balance_id", balance.ID).
			Msg("claimable: failed to dispatch near-expiry alert")
	}
}

// ExpiryFromPredicates returns the earliest absolute deadline across the
// claimants' predicates, if any predicate carries one.
func ExpiryFromPredicates(claimants []domain.Claimant, createdAt time.Time) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, c := range claimants {
		if t, ok := predicateDeadline(c.Predicate, createdAt); ok {
			if !found || t.Before(earliest) {
				earliest = t
				found = true
			}
		}
	}
	return earliest, found
}
