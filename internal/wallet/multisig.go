package wallet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// ThresholdLevel identifies which operation category a threshold applies to.
type ThresholdLevel string

const (
	ThresholdLow    ThresholdLevel = "low"    // trustlines, AllowTrust/ChangeTrust
	ThresholdMedium ThresholdLevel = "medium" // payments and most operations
	ThresholdHigh   ThresholdLevel = "high"   // signer changes, account options
)

var (
	ErrInvalidThreshold   = errors.New("invalid threshold configuration")
	ErrNoSigners          = errors.New("no signers configured")
	ErrQuorumNotMet       = errors.New("aggregate signature weight below threshold")
	ErrDuplicateSigner    = errors.New("duplicate signer")
	ErrUnknownSigner      = errors.New("signature from unknown signer")
	ErrAlreadySubmitted   = errors.New("transaction already submitted")
)

// Signer describes a co-signer and its weight on the account.
type Signer struct {
	Address string `json:"address"`
	Weight  uint32 `json:"weight"`
}

// Thresholds holds operational thresholds for a multisig account.
type Thresholds struct {
	MasterWeight uint32 `json:"master_weight"`
	Low          uint32 `json:"low"`
	Medium       uint32 `json:"medium"`
	High         uint32 `json:"high"`
}

// Validate checks threshold consistency: each threshold must be <= total weight
// available and must be non-zero.
func (t Thresholds) Validate(signers []Signer) error {
	if len(signers) == 0 {
		return ErrNoSigners
	}
	var total uint32 = t.MasterWeight
	for _, s := range signers {
		total += s.Weight
	}
	for name, v := range map[string]uint32{"low": t.Low, "medium": t.Medium, "high": t.High} {
		if v == 0 {
			return fmt.Errorf("%w: %s threshold must be > 0", ErrInvalidThreshold, name)
		}
		if v > total {
			return fmt.Errorf("%w: %s threshold %d exceeds total weight %d", ErrInvalidThreshold, name, v, total)
		}
	}
	if t.Low > t.Medium || t.Medium > t.High {
		return fmt.Errorf("%w: thresholds must satisfy low <= medium <= high", ErrInvalidThreshold)
	}
	return nil
}

// Config describes a multisig account configuration.
type Config struct {
	AccountID  string     `json:"account_id"`
	Signers    []Signer   `json:"signers"`
	Thresholds Thresholds `json:"thresholds"`
}

// Validate verifies the configuration is internally consistent.
func (c Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Signers))
	for _, s := range c.Signers {
		if _, err := keypair.ParseAddress(s.Address); err != nil {
			return fmt.Errorf("invalid signer address %q: %w", s.Address, err)
		}
		if _, dup := seen[s.Address]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateSigner, s.Address)
		}
		seen[s.Address] = struct{}{}
	}
	return c.Thresholds.Validate(c.Signers)
}

// Orchestrator manages multisig setup and signature collection.
type Orchestrator struct {
	horizon    horizonclient.ClientInterface
	passphrase string

	mu       sync.Mutex
	pending  map[string]*pendingTx // keyed by tx hash
}

type pendingTx struct {
	tx         *txnbuild.Transaction
	level      ThresholdLevel
	required   uint32
	signers    map[string]uint32 // address -> weight
	sigs       []xdr.DecoratedSignature
	submitted  bool
}

// NewOrchestrator creates a multisig orchestrator bound to a Horizon client.
func NewOrchestrator(h horizonclient.ClientInterface, passphrase string) *Orchestrator {
	return &Orchestrator{
		horizon:    h,
		passphrase: passphrase,
		pending:    make(map[string]*pendingTx),
	}
}

// BuildSetOptions creates the transaction that configures signers and
// thresholds on the account. It must be signed by the current master key
// (or existing high-threshold signers) and submitted by the caller.
func (o *Orchestrator) BuildSetOptions(sourceAccount txnbuild.Account, cfg Config) (*txnbuild.Transaction, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	master := cfg.Thresholds.MasterWeight
	low := cfg.Thresholds.Low
	med := cfg.Thresholds.Medium
	high := cfg.Thresholds.High

	ops := []txnbuild.Operation{
		&txnbuild.SetOptions{
			MasterWeight:    txnbuild.NewThreshold(txnbuild.Threshold(master)),
			LowThreshold:    txnbuild.NewThreshold(txnbuild.Threshold(low)),
			MediumThreshold: txnbuild.NewThreshold(txnbuild.Threshold(med)),
			HighThreshold:   txnbuild.NewThreshold(txnbuild.Threshold(high)),
		},
	}

	for _, s := range cfg.Signers {
		ops = append(ops, &txnbuild.SetOptions{
			Signer: &txnbuild.Signer{
				Address: s.Address,
				Weight:  txnbuild.Threshold(s.Weight),
			},
		})
	}

	return txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        sourceAccount,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewInfiniteTimeout(),
		},
	})
}

// requiredWeight returns the threshold weight needed for an operation level.
func requiredWeight(t Thresholds, level ThresholdLevel) (uint32, error) {
	switch level {
	case ThresholdLow:
		return t.Low, nil
	case ThresholdMedium:
		return t.Medium, nil
	case ThresholdHigh:
		return t.High, nil
	default:
		return 0, fmt.Errorf("%w: unknown level %q", ErrInvalidThreshold, level)
	}
}

// Propose registers a transaction for multi-party signature collection.
// The required weight is taken from cfg for the given level.
func (o *Orchestrator) Propose(cfg Config, tx *txnbuild.Transaction, level ThresholdLevel) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	required, err := requiredWeight(cfg.Thresholds, level)
	if err != nil {
		return "", err
	}
	hash, err := tx.HashHex(o.passphrase)
	if err != nil {
		return "", fmt.Errorf("hash transaction: %w", err)
	}
	weights := make(map[string]uint32, len(cfg.Signers))
	for _, s := range cfg.Signers {
		weights[s.Address] = s.Weight
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.pending[hash]; exists {
		return hash, nil
	}
	o.pending[hash] = &pendingTx{
		tx:       tx,
		level:    level,
		required: required,
		signers:  weights,
	}
	return hash, nil
}

// CollectSignature verifies and records a co-signer's signature on a pending
// transaction. It returns the aggregate weight collected so far.
func (o *Orchestrator) CollectSignature(txHash, signerAddress string, sig xdr.DecoratedSignature) (uint32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	p, ok := o.pending[txHash]
	if !ok {
		return 0, fmt.Errorf("unknown transaction %s", txHash)
	}
	if p.submitted {
		return 0, ErrAlreadySubmitted
	}
	weight, ok := p.signers[signerAddress]
	if !ok {
		return 0, ErrUnknownSigner
	}
	for _, existing := range p.sigs {
		if string(existing.Hint[:]) == string(sig.Hint[:]) && string(existing.Signature) == string(sig.Signature) {
			return o.aggregateLocked(p), nil
		}
	}

	hash, err := p.tx.Hash(o.passphrase)
	if err != nil {
		return 0, err
	}
	kp, err := keypair.ParseAddress(signerAddress)
	if err != nil {
		return 0, err
	}
	if err := kp.Verify(hash[:], sig.Signature); err != nil {
		return 0, fmt.Errorf("invalid signature from %s: %w", signerAddress, err)
	}

	p.sigs = append(p.sigs, sig)
	_ = weight
	return o.aggregateLocked(p), nil
}

// aggregateLocked sums the weight of signers who have contributed signatures.
// Caller must hold o.mu.
func (o *Orchestrator) aggregateLocked(p *pendingTx) uint32 {
	var total uint32
	for _, sig := range p.sigs {
		for addr, w := range p.signers {
			kp, err := keypair.ParseAddress(addr)
			if err != nil {
				continue
			}
			if strings.Equal(hintOf(kp), string(sig.Hint[:])) {
				total += w
				break
			}
		}
	}
	return total
}

// hintOf returns the 4-byte signature hint for a public key.
func hintOf(kp *keypair.FromAddress) string {
	raw := kp.Hint()
	return string(raw[:])
}

// Submit attaches collected signatures and submits to Horizon only when the
// aggregate weight meets the required threshold for the transaction's level.
func (o *Orchestrator) Submit(ctx context.Context, txHash string) (horizonResp string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	p, ok := o.pending[txHash]
	if !ok {
		return "", fmt.Errorf("unknown transaction %s", txHash)
	}
	if p.submitted {
		return "", ErrAlreadySubmitted
	}
	if got := o.aggregateLocked(p); got < p.required {
		return "", fmt.Errorf("%w: have %d, need %d", ErrQuorumNotMet, got, p.required)
	}

	signed := p.tx
	for _, sig := range p.sigs {
		signed, err = signed.AddSignatureDecorated(sig)
		if err != nil {
			return "", fmt.Errorf("attach signature: %w", err)
		}
	}

	resp, err := o.horizon.SubmitTransaction(signed)
	if err != nil {
		return "", fmt.Errorf("submit to horizon: %w", err)
	}
	p.submitted = true
	return resp.Hash, nil
}

// EncodeEnvelope returns the base64 XDR envelope of a pending transaction so
// co-signers can review and sign it out-of-band.
func (o *Orchestrator) EncodeEnvelope(txHash string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.pending[txHash]
	if !ok {
		return "", fmt.Errorf("unknown transaction %s", txHash)
	}
	env, err := p.tx.Base64()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(env)), nil
}
