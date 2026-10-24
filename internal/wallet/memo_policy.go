package wallet

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/Lumen-Nexora/Nexora/internal/tracing"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	// ErrInvalidMemoPolicy is returned when a memo policy fails validation.
	ErrInvalidMemoPolicy = errors.New("invalid memo policy")
	// ErrUnmappedDepositNotPending is returned when a queued deposit has
	// already been refunded or resolved.
	ErrUnmappedDepositNotPending = errors.New("unmapped deposit is no longer pending")
)

const (
	// MaxMemoLength is the number of bytes a Stellar text memo can hold.
	// The built-in memo formats are bounded by it because a customer
	// reference has to survive a round trip through the network.
	MaxMemoLength = 28

	// defaultMemoRefundAfter is the timeout applied when a tenant turns the
	// automated refund on without picking one.
	defaultMemoRefundAfter = 24 * time.Hour

	// maxMemoRefundAfter caps how long a deposit may sit in the
	// investigation queue before it is returned to its sender.
	maxMemoRefundAfter = 30 * 24 * time.Hour

	// memoPolicyCacheTTL bounds how long the indexer reuses a cached memo
	// policy. Memo validation runs on every inbound payment, so the policy
	// lookup has to stay off the database hot path, but a stale policy for
	// a few seconds is preferable to a query per payment.
	memoPolicyCacheTTL = 30 * time.Second

	// maxMemoPatternLength bounds a tenant-supplied memo pattern.
	maxMemoPatternLength = 512
)

// The built-in memo formats. A customer reference and a plain reference are
// deliberately distinct: the first identifies a customer of the platform, the
// second is an opaque token the tenant uses for its own bookkeeping.
var (
	customerIDMemoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,27}$`)
	emailMemoPattern      = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	phoneMemoPattern      = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)
	referenceMemoPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]{2,27}$`)
)

// MemoPolicyRepository persists the per-wallet inbound memo rules and the
// deposits that could not be mapped to a customer.
type MemoPolicyRepository interface {
	GetMemoPolicy(ctx context.Context, walletID string) (*domain.MemoPolicy, error)
	PutMemoPolicy(ctx context.Context, policy *domain.MemoPolicy) (*domain.MemoPolicy, error)
	DeleteMemoPolicy(ctx context.Context, walletID string) error
	ListMemoPolicies(ctx context.Context, tenantID string) ([]*domain.MemoPolicy, error)

	CreateUnmappedDeposit(ctx context.Context, deposit *domain.UnmappedDeposit) (bool, error)
	GetUnmappedDeposit(ctx context.Context, tenantID, id string) (*domain.UnmappedDeposit, error)
	ListUnmappedDeposits(ctx context.Context, tenantID, walletID, status string, limit, offset int) ([]*domain.UnmappedDeposit, int, error)
	ListUnmappedDepositsDueForRefund(ctx context.Context, now time.Time, limit int) ([]*domain.UnmappedDeposit, error)
	ClaimUnmappedDepositForRefund(ctx context.Context, id string) (bool, error)
	MarkUnmappedDepositRefunded(ctx context.Context, id, refundTxHash string) (bool, error)
	RecordUnmappedDepositRefundFailure(ctx context.Context, id, reason string) error
	RecordUnmappedDepositRefundUncertain(ctx context.Context, id, reason string) error
	MarkUnmappedDepositResolved(ctx context.Context, tenantID, id, resolution string) (bool, error)
}

// MemoTransferExecutor moves funds out of a custodial wallet to a Stellar
// account. It is the narrow slice of the wallet service the automated refund
// of an unmapped deposit needs.
type MemoTransferExecutor interface {
	ExecuteTransfer(ctx context.Context, walletID, destination, assetCode, issuer string, amount decimal.Decimal, memo string) (string, error)
}

// MemoAuditLogger records the audit trail for memo policies and for every
// unmapped deposit and its resolution.
type MemoAuditLogger interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
}

// memoPolicyCacheEntry is one cached policy lookup. The compiled matcher is
// kept next to the policy so a custom pattern is not recompiled per payment.
type memoPolicyCacheEntry struct {
	policy    *domain.MemoPolicy
	matcher   *regexp.Regexp
	expiresAt time.Time
}

// MemoPolicyService owns the inbound memo rules of a tenant's deposit
// accounts and the queue of deposits whose memo could not be matched to a
// customer.
//
// A wallet without a policy behaves exactly as before: every inbound payment
// is credited. A wallet with an enabled policy only credits payments whose
// memo satisfies the policy; everything else is filed as an unmapped deposit
// for investigation and, when the tenant asked for it, refunded to the
// sender once the policy's timeout expires.
type MemoPolicyService struct {
	repo     MemoPolicyRepository
	wallets  Repository
	executor MemoTransferExecutor
	audit    MemoAuditLogger
	now      func() time.Time

	mu    sync.RWMutex
	cache map[string]memoPolicyCacheEntry
}

// NewMemoPolicyService builds the memo policy service. wallets is used to
// prove that the tenant owns the wallet a policy is attached to; executor
// performs the automated refunds and may be nil when no refund is wanted.
func NewMemoPolicyService(repo MemoPolicyRepository, wallets Repository, executor MemoTransferExecutor, audit MemoAuditLogger) *MemoPolicyService {
	return &MemoPolicyService{
		repo:     repo,
		wallets:  wallets,
		executor: executor,
		audit:    audit,
		now:      func() time.Time { return time.Now().UTC() },
		cache:    make(map[string]memoPolicyCacheEntry),
	}
}

// GetPolicy returns the memo policy attached to a wallet.
func (s *MemoPolicyService) GetPolicy(ctx context.Context, walletID string) (*domain.MemoPolicy, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}
	policy, err := s.repo.GetMemoPolicy(ctx, walletID)
	if err != nil {
		return nil, err
	}
	if policy == nil || policy.TenantID != tenantID {
		return nil, domain.ErrMemoPolicyNotFound
	}
	return policy, nil
}

// PutPolicy creates or replaces the memo policy of a wallet.
func (s *MemoPolicyService) PutPolicy(ctx context.Context, policy *domain.MemoPolicy) (*domain.MemoPolicy, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}
	if policy == nil || strings.TrimSpace(policy.WalletID) == "" {
		return nil, ErrInvalidMemoPolicy
	}
	if !domain.ValidMemoFormat(policy.Format) {
		return nil, ErrInvalidMemoPolicy
	}
	if policy.Format == domain.MemoFormatCustom {
		if _, err := compileMemoPattern(policy.Pattern); err != nil {
			return nil, ErrInvalidMemoPolicy
		}
	} else {
		// A pattern only means something for the custom format; carrying it
		// around otherwise would suggest it is being enforced.
		policy.Pattern = ""
	}
	if policy.RefundAfter < 0 || policy.RefundAfter > maxMemoRefundAfter {
		return nil, ErrInvalidMemoPolicy
	}
	if policy.RefundAfter <= 0 {
		// The queue needs a positive timeout even when automated refunds
		// are off: the column records when the deposit becomes eligible.
		policy.RefundAfter = defaultMemoRefundAfter
	}

	// The policy is scoped to the tenant, so the wallet it names has to
	// belong to that tenant as well.
	if s.wallets != nil {
		w, err := s.wallets.GetByID(ctx, policy.WalletID)
		if err != nil {
			return nil, err
		}
		// A refund signs with the wallet's stored secret, which only a
		// custodial wallet has.
		if w.CustodyType != domain.CustodyCustodial {
			return nil, fmt.Errorf("%w: inbound memo policies apply to custodial wallets", ErrInvalidMemoPolicy)
		}
	}

	policy.TenantID = tenantID
	stored, err := s.repo.PutMemoPolicy(ctx, policy)
	if err != nil {
		return nil, err
	}
	s.invalidatePolicy(policy.WalletID)
	s.recordPolicyAudit(ctx, stored, "wallet.memo_policy_updated")
	return stored, nil
}

// DeletePolicy removes the memo policy of a wallet.
func (s *MemoPolicyService) DeletePolicy(ctx context.Context, walletID string) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return domain.ErrForbidden
	}
	policy, err := s.repo.GetMemoPolicy(ctx, walletID)
	if err != nil {
		return err
	}
	if policy == nil || policy.TenantID != tenantID {
		return domain.ErrMemoPolicyNotFound
	}
	if err := s.repo.DeleteMemoPolicy(ctx, walletID); err != nil {
		return err
	}
	s.invalidatePolicy(walletID)
	s.recordPolicyAudit(ctx, policy, "wallet.memo_policy_deleted")
	return nil
}

// ListPolicies returns every memo policy of the tenant.
func (s *MemoPolicyService) ListPolicies(ctx context.Context) ([]*domain.MemoPolicy, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}
	return s.repo.ListMemoPolicies(ctx, tenantID)
}

// EvaluateMemo matches an inbound memo against the wallet's memo policy. A
// nil evaluation means the wallet has no enabled policy, so the deposit is
// mapped the usual way.
//
// A policy that does not require a memo still validates the ones that are
// sent: "require_memo" only decides whether a payment without a memo is
// accepted, not whether the format is enforced.
func (s *MemoPolicyService) EvaluateMemo(ctx context.Context, walletID, memo string) (*domain.MemoEvaluation, error) {
	policy, matcher, err := s.policyFor(ctx, walletID)
	if err != nil {
		return nil, err
	}
	if policy == nil || !policy.Enabled {
		return nil, nil
	}
	trimmed := strings.TrimSpace(memo)
	if trimmed == "" {
		if !policy.RequireMemo {
			return &domain.MemoEvaluation{
				PolicyID: policy.ID,
				Format:   policy.Format,
				Matched:  true,
			}, nil
		}
		return &domain.MemoEvaluation{
			PolicyID: policy.ID,
			Format:   policy.Format,
			Matched:  false,
			Reason:   domain.UnmappedDepositReasonMissing,
		}, nil
	}
	if customerID, ok := matchMemo(policy, matcher, trimmed); ok {
		return &domain.MemoEvaluation{
			PolicyID:   policy.ID,
			Format:     policy.Format,
			Matched:    true,
			CustomerID: customerID,
		}, nil
	}
	return &domain.MemoEvaluation{
		PolicyID: policy.ID,
		Format:   policy.Format,
		Matched:  false,
		Reason:   domain.UnmappedDepositReasonInvalid,
	}, nil
}

// RecordUnmappedDeposit files a deposit whose memo could not be matched into
// the pending investigation queue and writes the matching audit event.
func (s *MemoPolicyService) RecordUnmappedDeposit(ctx context.Context, deposit *domain.UnmappedDeposit) error {
	if deposit == nil {
		return nil
	}
	now := s.now()
	if deposit.ID == "" {
		deposit.ID = uuid.NewString()
	}
	if deposit.TenantID == "" {
		deposit.TenantID = tenant.IDFromContext(ctx)
	}
	if deposit.CreatedAt.IsZero() {
		deposit.CreatedAt = now
	}
	deposit.UpdatedAt = now
	if deposit.Status == "" {
		deposit.Status = domain.UnmappedDepositPending
	}
	if deposit.Reason == "" {
		deposit.Reason = domain.UnmappedDepositReasonInvalid
	}
	if !deposit.Mode.Valid() {
		deposit.Mode = domain.ModeLive
	}
	if deposit.RefundAfter.IsZero() {
		deposit.RefundAfter = now.Add(defaultMemoRefundAfter)
		if policy, _, err := s.policyFor(ctx, deposit.WalletID); err == nil && policy != nil && policy.RefundAfter > 0 {
			deposit.RefundAfter = now.Add(policy.RefundAfter)
		}
	}
	inserted, err := s.repo.CreateUnmappedDeposit(ctx, deposit)
	if err != nil {
		return fmt.Errorf("record unmapped deposit: %w", err)
	}
	if !inserted {
		// The indexer replayed a payment that is already queued: its audit
		// trail must not be written twice.
		return nil
	}
	s.recordDepositAudit(ctx, deposit, "deposit.memo_unmapped", map[string]interface{}{
		"tx_hash":        deposit.TxHash,
		"wallet_id":      deposit.WalletID,
		"source_address": deposit.SourceAddress,
		"asset":          deposit.Asset,
		"amount":         deposit.Amount.String(),
		"memo":           deposit.Memo,
		"reason":         string(deposit.Reason),
	})
	return nil
}

// ListUnmappedDeposits returns the tenant's pending investigation queue.
func (s *MemoPolicyService) ListUnmappedDeposits(ctx context.Context, walletID, status string, limit, offset int) ([]*domain.UnmappedDeposit, int, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, 0, domain.ErrForbidden
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.ListUnmappedDeposits(ctx, tenantID, walletID, status, limit, offset)
}

// ResolveUnmappedDeposit closes a queued deposit with the operator's
// resolution note.
func (s *MemoPolicyService) ResolveUnmappedDeposit(ctx context.Context, id, resolution string) (*domain.UnmappedDeposit, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(resolution) == "" {
		return nil, ErrInvalidMemoPolicy
	}
	deposit, err := s.repo.GetUnmappedDeposit(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if deposit == nil {
		return nil, domain.ErrUnmappedDepositNotFound
	}
	if deposit.Status != domain.UnmappedDepositPending {
		return nil, ErrUnmappedDepositNotPending
	}
	resolved, err := s.repo.MarkUnmappedDepositResolved(ctx, tenantID, id, resolution)
	if err != nil {
		return nil, err
	}
	if !resolved {
		// The deposit left the pending state between the read and the
		// update: a refund sweep claimed it in the meantime.
		return nil, ErrUnmappedDepositNotPending
	}
	deposit.Status = domain.UnmappedDepositResolved
	deposit.Resolution = resolution
	resolvedAt := s.now()
	deposit.ResolvedAt = &resolvedAt
	s.recordDepositAudit(ctx, deposit, "deposit.memo_resolved", map[string]interface{}{
		"tx_hash":    deposit.TxHash,
		"resolution": resolution,
	})
	return deposit, nil
}

// RefundDueUnmappedDeposits returns the deposits that waited longer than
// their policy's refund timeout to their sender and returns how many were
// refunded.
//
// A deposit is claimed before its refund is sent, so two workers sweeping
// the same queue never return the same deposit twice. A refund that cannot
// be sent is recorded on the deposit and leaves it pending: the next sweep
// retries it, and the failure reason stays visible to whoever investigates
// the queue.
func (s *MemoPolicyService) RefundDueUnmappedDeposits(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	deposits, err := s.repo.ListUnmappedDepositsDueForRefund(ctx, s.now(), limit)
	if err != nil {
		return 0, fmt.Errorf("list unmapped deposits due for refund: %w", err)
	}
	refunded := 0
	for _, deposit := range deposits {
		claimed, err := s.repo.ClaimUnmappedDepositForRefund(ctx, deposit.ID)
		if err != nil {
			return refunded, err
		}
		if !claimed {
			// Another worker took the deposit; it is not ours to refund.
			continue
		}
		if err := s.refundDeposit(ctx, deposit); err != nil {
			if errors.Is(err, errRefundSent) {
				// The transfer went out but the queue entry could not be
				// closed. The deposit must not go back to pending: the money
				// already left the wallet, and the next sweep would send it
				// a second time. The row stays in the refunding state with
				// the reason recorded so an operator can reconcile it.
				if recordErr := s.repo.RecordUnmappedDepositRefundUncertain(ctx, deposit.ID, err.Error()); recordErr != nil {
					return refunded, fmt.Errorf("record the uncertain refund of %s: %w", deposit.ID, recordErr)
				}
				continue
			}
			if recordErr := s.repo.RecordUnmappedDepositRefundFailure(ctx, deposit.ID, err.Error()); recordErr != nil {
				return refunded, fmt.Errorf("record refund failure for %s: %w", deposit.ID, recordErr)
			}
			continue
		}
		refunded++
	}
	return refunded, nil
}

// errRefundSent marks a refund whose transfer went out but whose queue entry
// could not be closed. The deposit must not be sent back to pending: the
// money already left the wallet.
var errRefundSent = errors.New("the refund was sent but the queue entry could not be closed")

// refundDeposit sends an unmapped deposit back to the account it came from.
func (s *MemoPolicyService) refundDeposit(ctx context.Context, deposit *domain.UnmappedDeposit) error {
	if s.executor == nil {
		return errors.New("memo refund executor is not configured")
	}
	if deposit.SourceAddress == "" {
		return errors.New("unmapped deposit has no source address to refund")
	}
	if !deposit.Amount.IsPositive() {
		return errors.New("unmapped deposit has no amount to refund")
	}

	// The refund runs against a specific tenant and environment: rehydrate
	// both from the persisted row so a test-mode deposit is never refunded
	// on the live network.
	depositCtx := tenant.WithID(ctx, deposit.TenantID)
	if deposit.Mode.Valid() {
		depositCtx = tenant.WithMode(depositCtx, deposit.Mode)
	}

	txHash, err := s.executor.ExecuteTransfer(
		depositCtx,
		deposit.WalletID,
		deposit.SourceAddress,
		deposit.Asset,
		deposit.Issuer,
		deposit.Amount,
		"deposit-refund",
	)
	if err != nil {
		return fmt.Errorf("refund %s to %s: %w", deposit.TxHash, deposit.SourceAddress, err)
	}
	marked, err := s.repo.MarkUnmappedDepositRefunded(ctx, deposit.ID, txHash)
	if err != nil {
		return fmt.Errorf("%w: mark deposit %s refunded: %v", errRefundSent, deposit.ID, err)
	}
	if !marked {
		return fmt.Errorf("%w: deposit %s left the refunding state", errRefundSent, deposit.ID)
	}
	deposit.Status = domain.UnmappedDepositRefunded
	deposit.RefundTxHash = txHash
	s.recordDepositAudit(ctx, deposit, "deposit.memo_refunded", map[string]interface{}{
		"tx_hash":        deposit.TxHash,
		"refund_tx_hash": txHash,
		"source_address": deposit.SourceAddress,
		"asset":          deposit.Asset,
		"amount":         deposit.Amount.String(),
	})
	return nil
}

// policyFor returns the memo policy of a wallet together with the matcher
// for its format. A nil policy means the wallet has none.
func (s *MemoPolicyService) policyFor(ctx context.Context, walletID string) (*domain.MemoPolicy, *regexp.Regexp, error) {
	if walletID == "" {
		return nil, nil, nil
	}
	s.mu.RLock()
	entry, cached := s.cache[walletID]
	s.mu.RUnlock()
	if cached && s.now().Before(entry.expiresAt) {
		return entry.policy, entry.matcher, nil
	}

	policy, err := s.repo.GetMemoPolicy(ctx, walletID)
	if err != nil {
		return nil, nil, err
	}
	var matcher *regexp.Regexp
	if policy != nil && policy.Format == domain.MemoFormatCustom {
		matcher, err = compileMemoPattern(policy.Pattern)
		if err != nil {
			// A stored pattern that no longer compiles must not stop the
			// wallet from being validated: the memo simply fails to match
			// and lands in the investigation queue.
			matcher = nil
		}
	}
	s.mu.Lock()
	s.cache[walletID] = memoPolicyCacheEntry{
		policy:    policy,
		matcher:   matcher,
		expiresAt: s.now().Add(memoPolicyCacheTTL),
	}
	s.mu.Unlock()
	return policy, matcher, nil
}

// invalidatePolicy drops a cached policy so a change takes effect at once.
func (s *MemoPolicyService) invalidatePolicy(walletID string) {
	s.mu.Lock()
	delete(s.cache, walletID)
	s.mu.Unlock()
}

// matchMemo tests a non-empty memo against the policy's expected format and
// returns the customer reference it carries.
func matchMemo(policy *domain.MemoPolicy, matcher *regexp.Regexp, memo string) (string, bool) {
	trimmed := strings.TrimSpace(memo)
	if trimmed == "" {
		return "", false
	}
	switch policy.Format {
	case domain.MemoFormatCustomerID:
		if len(trimmed) <= MaxMemoLength && customerIDMemoPattern.MatchString(trimmed) {
			return trimmed, true
		}
	case domain.MemoFormatEmail:
		if len(trimmed) <= MaxMemoLength && emailMemoPattern.MatchString(trimmed) {
			return trimmed, true
		}
	case domain.MemoFormatPhone:
		if len(trimmed) <= MaxMemoLength && phoneMemoPattern.MatchString(trimmed) {
			return trimmed, true
		}
	case domain.MemoFormatReference:
		if len(trimmed) <= MaxMemoLength && referenceMemoPattern.MatchString(trimmed) {
			return trimmed, true
		}
	case domain.MemoFormatCustom:
		// The tenant's pattern decides what a valid memo looks like, so
		// only the custom format escapes the Stellar memo length budget.
		if matcher != nil && matcher.MatchString(trimmed) {
			return trimmed, true
		}
	}
	return "", false
}

// compileMemoPattern compiles a tenant-supplied memo pattern. The pattern is
// anchored so that it has to describe the whole memo.
func compileMemoPattern(pattern string) (*regexp.Regexp, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" {
		return nil, errors.New("a memo pattern is required for the custom format")
	}
	if len(trimmed) > maxMemoPatternLength {
		return nil, errors.New("the memo pattern is too long")
	}
	compiled, err := regexp.Compile("^(?:" + trimmed + ")$")
	if err != nil {
		return nil, fmt.Errorf("compile the memo pattern: %w", err)
	}
	return compiled, nil
}

// recordPolicyAudit writes the audit event for a memo policy change.
func (s *MemoPolicyService) recordPolicyAudit(ctx context.Context, policy *domain.MemoPolicy, action string) {
	if s.audit == nil || policy == nil {
		return
	}
	metadata := map[string]interface{}{}
	metadata["wallet_id"] = policy.WalletID
	metadata["format"] = string(policy.Format)
	metadata["auto_refund"] = policy.AutoRefund
	metadata["refund_after_seconds"] = int64(policy.RefundAfter / time.Second)
	actorType, actorID := memoAuditActor(ctx)
	event := &domain.AuditEvent{
		ID:           uuid.NewString(),
		TenantID:     policy.TenantID,
		Mode:         tenant.ModeOrDefault(ctx, domain.ModeLive),
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: "wallet_memo_policy",
		ResourceID:   policy.ID,
		Metadata:     metadata,
		CreatedAt:    s.now(),
	}
	if err := s.audit.Record(ctx, event); err != nil {
		logMemoPolicyError(ctx, action, policy.ID, err)
	}
}

// recordDepositAudit writes the audit event for an unmapped deposit and its
// resolution.
func (s *MemoPolicyService) recordDepositAudit(ctx context.Context, deposit *domain.UnmappedDeposit, action string, metadata map[string]interface{}) {
	if s.audit == nil || deposit == nil {
		return
	}
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	metadata["status"] = string(deposit.Status)
	actorType, actorID := memoAuditActor(ctx)
	event := &domain.AuditEvent{
		ID:           uuid.NewString(),
		TenantID:     deposit.TenantID,
		Mode:         deposit.Mode,
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: "unmapped_deposit",
		ResourceID:   deposit.ID,
		Metadata:     metadata,
		CreatedAt:    s.now(),
	}
	if err := s.audit.Record(ctx, event); err != nil {
		logMemoPolicyError(ctx, action, deposit.ID, err)
	}
}

// memoAuditActor resolves the actor behind an audit event: the dashboard user
// when the request was authenticated with a JWT, the API key when it was not,
// and the system otherwise.
func memoAuditActor(ctx context.Context) (string, string) {
	if userID := tenant.UserIDFromContext(ctx); userID != "" {
		return "user", userID
	}
	if keyID := tenant.APIKeyIDFromContext(ctx); keyID != "" {
		return "api_key", keyID
	}
	return "system", "system"
}

// logMemoPolicyError records a failed audit write. The audit trail is
// best-effort: a memo policy change must not be rejected because its audit
// row could not be written, but the failure still has to be visible.
func logMemoPolicyError(ctx context.Context, action, resourceID string, err error) {
	tracing.Logger(ctx).Error().Err(err).
		Str("action", action).
		Str("resource_id", resourceID).
		Msg("wallet: memo policy audit event failed")
}
