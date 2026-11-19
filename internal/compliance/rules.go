package compliance

import (
	"fmt"
	"math"
	"time"
)

// Typology identifies a known financial crime pattern.
type Typology string

const (
	TypologyPassThrough  Typology = "PASS_THROUGH"
	TypologyStructuring  Typology = "STRUCTURING"
	TypologyRoundNumbers Typology = "ROUND_NUMBERS"
	TypologyRapidMovement Typology = "RAPID_MOVEMENT"
)

// Direction of a transaction relative to the customer account.
type Direction string

const (
	DirectionDeposit    Direction = "DEPOSIT"
	DirectionWithdrawal Direction = "WITHDRAWAL"
)

// Transaction is a single account movement evaluated by the engine.
type Transaction struct {
	ID         string
	CustomerID string
	Amount     float64
	Currency   string
	Direction  Direction
	Counterparty string
	Timestamp  time.Time
}

// Finding is the result of a rule matching a set of transactions.
type Finding struct {
	Typology     Typology
	CustomerID   string
	Transactions []Transaction
	Score        float64
	Reason       string
}

// Rule evaluates a customer's transaction history for a typology.
type Rule interface {
	Typology() Typology
	Evaluate(customerID string, txns []Transaction) []Finding
}

// RuleConfig holds tunable thresholds for the default rule set.
type RuleConfig struct {
	// StructuringThreshold is the reporting threshold being evaded (e.g. 10000).
	StructuringThreshold float64
	// StructuringBand is the fraction below threshold considered suspicious (e.g. 0.10).
	StructuringBand float64
	// StructuringMinCount is the minimum number of sub-threshold deposits.
	StructuringMinCount int
	// StructuringWindow is the lookback window for structuring detection.
	StructuringWindow time.Duration

	// RoundNumberMinAmount is the minimum amount considered for round-number checks.
	RoundNumberMinAmount float64
	// RoundNumberModulus is the divisor that defines a round amount (e.g. 1000).
	RoundNumberModulus float64
	// RoundNumberMinCount is the number of round transactions needed to flag.
	RoundNumberMinCount int

	// PassThroughWindow is the max time between deposit and withdrawal.
	PassThroughWindow time.Duration
	// PassThroughTolerance is the relative amount difference allowed (e.g. 0.05).
	PassThroughTolerance float64
	// PassThroughMinAmount ignores trivially small movements.
	PassThroughMinAmount float64
}

// DefaultRuleConfig returns conservative default thresholds.
func DefaultRuleConfig() RuleConfig {
	return RuleConfig{
		StructuringThreshold: 10000,
		StructuringBand:      0.10,
		StructuringMinCount:  3,
		StructuringWindow:    72 * time.Hour,

		RoundNumberMinAmount: 5000,
		RoundNumberModulus:   1000,
		RoundNumberMinCount:  4,

		PassThroughWindow:    48 * time.Hour,
		PassThroughTolerance: 0.05,
		PassThroughMinAmount: 1000,
	}
}

// StructuringRule detects repeated deposits just below a reporting threshold.
type StructuringRule struct{ cfg RuleConfig }

func (r StructuringRule) Typology() Typology { return TypologyStructuring }

func (r StructuringRule) Evaluate(customerID string, txns []Transaction) []Finding {
	lower := r.cfg.StructuringThreshold * (1 - r.cfg.StructuringBand)
	upper := r.cfg.StructuringThreshold

	var matched []Transaction
	for _, t := range txns {
		if t.Direction != DirectionDeposit {
			continue
		}
		if t.Amount >= lower && t.Amount < upper {
			matched = append(matched, t)
		}
	}

	// Slide a window over matched transactions.
	var findings []Finding
	for i := range matched {
		var group []Transaction
		for j := i; j < len(matched); j++ {
			if matched[j].Timestamp.Sub(matched[i].Timestamp) > r.cfg.StructuringWindow {
				break
			}
			group = append(group, matched[j])
		}
		if len(group) >= r.cfg.StructuringMinCount {
			findings = append(findings, Finding{
				Typology:     TypologyStructuring,
				CustomerID:   customerID,
				Transactions: group,
				Score:        math.Min(1.0, float64(len(group))/float64(r.cfg.StructuringMinCount+2)),
				Reason: fmt.Sprintf("%d deposits between %.2f and %.2f within %s",
					len(group), lower, upper, r.cfg.StructuringWindow),
			})
			break
		}
	}
	return findings
}

// RoundNumberRule detects a high frequency of round-amount transactions.
type RoundNumberRule struct{ cfg RuleConfig }

func (r RoundNumberRule) Typology() Typology { return TypologyRoundNumbers }

func (r RoundNumberRule) Evaluate(customerID string, txns []Transaction) []Finding {
	var round []Transaction
	for _, t := range txns {
		if t.Amount < r.cfg.RoundNumberMinAmount {
			continue
		}
		rem := math.Mod(t.Amount, r.cfg.RoundNumberModulus)
		if rem < 1e-9 || r.cfg.RoundNumberModulus-rem < 1e-9 {
			round = append(round, t)
		}
	}
	if len(round) < r.cfg.RoundNumberMinCount {
		return nil
	}
	return []Finding{{
		Typology:     TypologyRoundNumbers,
		CustomerID:   customerID,
		Transactions: round,
		Score:        math.Min(1.0, float64(len(round))/float64(r.cfg.RoundNumberMinCount*2)),
		Reason:       fmt.Sprintf("%d round-amount transactions (multiples of %.0f)", len(round), r.cfg.RoundNumberModulus),
	}}
}

// PassThroughRule flags deposits rapidly followed by a withdrawal of nearly equal value.
type PassThroughRule struct{ cfg RuleConfig }

func (r PassThroughRule) Typology() Typology { return TypologyPassThrough }

func (r PassThroughRule) Evaluate(customerID string, txns []Transaction) []Finding {
	var findings []Finding
	for i, dep := range txns {
		if dep.Direction != DirectionDeposit || dep.Amount < r.cfg.PassThroughMinAmount {
			continue
		}
		for j := i + 1; j < len(txns); j++ {
			wd := txns[j]
			if wd.Timestamp.Sub(dep.Timestamp) > r.cfg.PassThroughWindow {
				break
			}
			if wd.Direction != DirectionWithdrawal || wd.Currency != dep.Currency {
				continue
			}
			diff := math.Abs(wd.Amount-dep.Amount) / dep.Amount
			if diff <= r.cfg.PassThroughTolerance {
				findings = append(findings, Finding{
					Typology:     TypologyPassThrough,
					CustomerID:   customerID,
					Transactions: []Transaction{dep, wd},
					Score:        1 - diff/r.cfg.PassThroughTolerance*0.5,
					Reason: fmt.Sprintf("deposit %.2f %s followed by withdrawal %.2f within %s",
						dep.Amount, dep.Currency, wd.Amount, r.cfg.PassThroughWindow),
				})
				break
			}
		}
	}
	return findings
}

// DefaultRules returns the standard typology rule set.
func DefaultRules(cfg RuleConfig) []Rule {
	return []Rule{
		StructuringRule{cfg: cfg},
		RoundNumberRule{cfg: cfg},
		PassThroughRule{cfg: cfg},
	}
}
