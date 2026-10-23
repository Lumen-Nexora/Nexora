package reconcile

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// ResolutionAction enumerates the structured resolution types an operator may
// apply to a reconciliation discrepancy.
type ResolutionAction string

const (
	ActionWriteOff          ResolutionAction = "write_off"
	ActionManualAdjustment  ResolutionAction = "manual_adjustment"
	ActionPartnerRecovery   ResolutionAction = "partner_recovery"
)

// MakerCheckerThreshold is the amount (in major currency units) above which an
// adjustment requires a second, independent approver.
var MakerCheckerThreshold = mustDecimal("50")

// ErrMakerCheckerRequired is returned when a large adjustment lacks a valid checker.
var ErrMakerCheckerRequired = errors.New("adjustments above threshold require a distinct checker approval")

// ErrSelfApproval is returned when the maker attempts to approve their own request.
var ErrSelfApproval = errors.New("maker cannot approve their own resolution")

// ErrInvalidResolutionAction is returned for unknown resolution action types.
var ErrInvalidResolutionAction = errors.New("unsupported resolution action")

// ResolutionRequest describes an operator's proposed resolution of a discrepancy.
type ResolutionRequest struct {
	DiscrepancyID string
	TenantID      string
	Action        ResolutionAction
	Amount        string // decimal string, e.g. "125.50"
	Asset         string
	Note          string
	MakerID       string
	CheckerID     string // required when amount > threshold
	CheckerSig    string // HMAC signature over the request payload by the checker
	RequestedAt   time.Time
}

// ResolutionResult is the outcome of an applied resolution.
type ResolutionResult struct {
	DiscrepancyID string
	Action        ResolutionAction
	JournalDebit  string
	JournalCredit string
	AppliedAt     time.Time
	AuditHash     string
}

// JournalEntry is a single balanced double-entry posting.
type JournalEntry struct {
	TenantID   string
	Account    string
	Debit      string
	Credit     string
	Asset      string
	Reference  string
	PostedAt   time.Time
}

// LedgerPoster persists balanced journal entries atomically.
type LedgerPoster interface {
	PostBalanced(ctx context.Context, entries []JournalEntry) error
}

// DiscrepancyResolver is the persistence port for discrepancy state transitions.
type DiscrepancyResolver interface {
	MarkResolved(ctx context.Context, tenantID, id, note string) error
	RecordAudit(ctx context.Context, tenantID, id, actorID string, action ResolutionAction, payloadHash, checkerID string, at time.Time) error
}

// Workflow coordinates maker-checker approval, journal posting, and audit trail.
type Workflow struct {
	ledger     LedgerPoster
	resolver   DiscrepancyResolver
	signingKey []byte
	now        func() time.Time
}

// NewWorkflow constructs a Workflow. signingKey is used to verify checker approvals.
func NewWorkflow(ledger LedgerPoster, resolver DiscrepancyResolver, signingKey []byte) (*Workflow, error) {
	if ledger == nil || resolver == nil {
		return nil, errors.New("ledger and resolver are required")
	}
	if len(signingKey) < 32 {
		return nil, errors.New("signing key must be at least 32 bytes")
	}
	return &Workflow{ledger: ledger, resolver: resolver, signingKey: signingKey, now: func() time.Time { return time.Now().UTC() }}, nil
}

// PayloadDigest returns the canonical digest that a checker must sign.
func PayloadDigest(req ResolutionRequest) string {
	canonical := strings.Join([]string{
		req.TenantID, req.DiscrepancyID, string(req.Action), req.Amount, req.Asset, req.MakerID, req.Note,
	}, "|")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// SignApproval produces the checker signature for a request. Exposed so the
// approval endpoint and tests share one implementation.
func (w *Workflow) SignApproval(req ResolutionRequest) string {
	mac := hmac.New(sha256.New, w.signingKey)
	mac.Write([]byte(PayloadDigest(req)))
	mac.Write([]byte(req.CheckerID))
	return hex.EncodeToString(mac.Sum(nil))
}

// Resolve validates the request, enforces maker-checker controls, posts balancing
// journal entries, marks the discrepancy resolved, and records the audit trail.
func (w *Workflow) Resolve(ctx context.Context, req ResolutionRequest) (*ResolutionResult, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	amount, err := parseAmount(req.Amount)
	if err != nil {
		return nil, err
	}

	if amount.Cmp(MakerCheckerThreshold) > 0 {
		if req.CheckerID == "" || req.CheckerID == req.MakerID {
			return nil, ErrMakerCheckerRequired
		}
		expected := w.SignApproval(req)
		if !hmac.Equal([]byte(expected), []byte(req.CheckerSig)) {
			return nil, fmt.Errorf("%w: invalid checker signature", ErrMakerCheckerRequired)
		}
	}

	entries, debitAcct, creditAcct, err := buildBalancingEntries(req, amount, w.now())
	if err != nil {
		return nil, err
	}
	if err := w.ledger.PostBalanced(ctx, entries); err != nil {
		return nil, fmt.Errorf("post journal entries: %w", err)
	}

	now := w.now()
	digest := PayloadDigest(req)
	if err := w.resolver.MarkResolved(ctx, req.TenantID, req.DiscrepancyID, req.Note); err != nil {
		return nil, fmt.Errorf("mark discrepancy resolved: %w", err)
	}
	if err := w.resolver.RecordAudit(ctx, req.TenantID, req.DiscrepancyID, req.MakerID, req.Action, digest, req.CheckerID, now); err != nil {
		return nil, fmt.Errorf("record audit trail: %w", err)
	}

	return &ResolutionResult{
		DiscrepancyID: req.DiscrepancyID,
		Action:        req.Action,
		JournalDebit:  debitAcct,
		JournalCredit: creditAcct,
		AppliedAt:     now,
		AuditHash:     digest,
	}, nil
}

func validateRequest(req ResolutionRequest) error {
	switch req.Action {
	case ActionWriteOff, ActionManualAdjustment, ActionPartnerRecovery:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidResolutionAction, req.Action)
	}
	if req.TenantID == "" || req.DiscrepancyID == "" || req.MakerID == "" {
		return errors.New("tenant, discrepancy, and maker identity are required")
	}
	if req.Asset == "" {
		return errors.New("asset is required")
	}
	if strings.TrimSpace(req.Note) == "" {
		return errors.New("resolution note is required")
	}
	if req.CheckerID != "" && req.CheckerID == req.MakerID {
		return ErrSelfApproval
	}
	return nil
}

// buildBalancingEntries produces a debit/credit pair that always balances.
func buildBalancingEntries(req ResolutionRequest, amount amountT, at time.Time) ([]JournalEntry, string, string, error) {
	var debitAcct, creditAcct string
	switch req.Action {
	case ActionWriteOff:
		debitAcct, creditAcct = "expense:reconciliation_writeoff", "suspense:reconciliation"
	case ActionManualAdjustment:
		debitAcct, creditAcct = "suspense:reconciliation", "ledger:adjustment_clearing"
	case ActionPartnerRecovery:
		debitAcct, creditAcct = "receivable:partner", "suspense:reconciliation"
	default:
		return nil, "", "", ErrInvalidResolutionAction
	}
	amt := amount.String()
	ref := req.DiscrepancyID
	return []JournalEntry{
		{TenantID: req.TenantID, Account: debitAcct, Debit: amt, Credit: "0", Asset: req.Asset, Reference: ref, PostedAt: at},
		{TenantID: req.TenantID, Account: creditAcct, Debit: "0", Credit: amt, Asset: req.Asset, Reference: ref, PostedAt: at},
	}, debitAcct, creditAcct, nil
}

var _ = domain.ErrConcurrentUpdate
