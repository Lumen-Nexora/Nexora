package reconcile

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// RootCause classifies why an internal ledger balance diverges from on-chain.
type RootCause string

const (
	CausePendingSettlement   RootCause = "pending_settlement"
	CauseFeeSlippage         RootCause = "fee_slippage"
	CauseUnindexedDeposit    RootCause = "unindexed_deposit"
	CauseUnauthorizedTransfer RootCause = "unauthorized_transfer"
	CauseUnknown             RootCause = "unknown"
)

// Discrepancy describes a single account whose internal and on-chain balances differ.
type Discrepancy struct {
	AccountID     string
	AssetCode     string
	Internal      *big.Rat
	OnChain       *big.Rat
	Variance      *big.Rat
	RootCause     RootCause
	FirstSeenAt   time.Time
	DetectedAt    time.Time
}

// BalanceSnapshot is a point-in-time balance for one account/asset pair.
type BalanceSnapshot struct {
	AccountID string
	AssetCode string
	Balance   *big.Rat
}

// PendingState carries context used to attribute a variance to a root cause.
type PendingState struct {
	HasPendingSettlement bool
	ExpectedFee          *big.Rat
	ObservedFee          *big.Rat
	UnindexedDeposit     *big.Rat
	UnauthorizedTransfer bool
}

// Detector compares snapshots and produces discrepancies beyond a zero tolerance.
type Detector struct {
	Tolerance *big.Rat
	Now       func() time.Time
}

// NewDetector returns a Detector with zero tolerance and wall-clock time.
func NewDetector() *Detector {
	return &Detector{Tolerance: new(big.Rat), Now: time.Now}
}

// Compare returns discrepancies where |internal - onChain| exceeds tolerance.
func (d *Detector) Compare(internal, onChain map[string]BalanceSnapshot, pending map[string]PendingState) []Discrepancy {
	now := d.Now()
	var out []Discrepancy
	for key, ledger := range internal {
		chain, ok := onChain[key]
		if !ok {
			chain = BalanceSnapshot{AccountID: ledger.AccountID, AssetCode: ledger.AssetCode, Balance: new(big.Rat)}
		}
		variance := new(big.Rat).Sub(ledger.Balance, chain.Balance)
		abs := new(big.Rat).Abs(variance)
		if abs.Cmp(d.Tolerance) <= 0 {
			continue
		}
		out = append(out, Discrepancy{
			AccountID:  ledger.AccountID,
			AssetCode:  ledger.AssetCode,
			Internal:   new(big.Rat).Set(ledger.Balance),
			OnChain:    new(big.Rat).Set(chain.Balance),
			Variance:   variance,
			RootCause:  Classify(variance, pending[key]),
			DetectedAt: now,
		})
	}
	return out
}

// Classify attributes a variance to a root cause using pending context.
func Classify(variance *big.Rat, p PendingState) RootCause {
	switch {
	case p.UnauthorizedTransfer:
		return CauseUnauthorizedTransfer
	case p.UnindexedDeposit != nil && p.UnindexedDeposit.Cmp(new(big.Rat).Abs(variance)) == 0:
		return CauseUnindexedDeposit
	case p.ExpectedFee != nil && p.ObservedFee != nil && p.ExpectedFee.Cmp(p.ObservedFee) != 0:
		diff := new(big.Rat).Sub(p.ExpectedFee, p.ObservedFee)
		if diff.Cmp(new(big.Rat).Abs(variance)) == 0 {
			return CauseFeeSlippage
		}
	case p.HasPendingSettlement:
		return CausePendingSettlement
	}
	return CauseUnknown
}

// ErrInvalidTolerance is returned when a negative tolerance is configured.
var ErrInvalidTolerance = errors.New("reconcile: tolerance must be non-negative")

// Validate checks detector configuration.
func (d *Detector) Validate() error {
	if d.Tolerance == nil || d.Tolerance.Sign() < 0 {
		return ErrInvalidTolerance
	}
	return nil
}

func (d Discrepancy) String() string {
	return fmt.Sprintf("%s/%s variance=%s cause=%s", d.AccountID, d.AssetCode, d.Variance.FloatString(7), d.RootCause)
}
