package transferapproval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var ErrInvalidPolicy = errors.New("invalid transfer approval policy")

type Repository interface {
	GetPolicy(context.Context, string, string) (*domain.TransferApprovalPolicy, error)
	PutPolicy(context.Context, *domain.TransferApprovalPolicy) error
	CreateRequest(context.Context, *domain.TransferApprovalRequest) error
	ListRequests(context.Context, string, string, int, int) ([]*domain.TransferApprovalRequest, int, error)
	CastVote(context.Context, string, string, string, string, string) (*domain.TransferApprovalRequest, bool, error)
}

type Enqueuer interface {
	EnqueueTransfer(context.Context, string) error
}

type Service struct {
	repo  Repository
	queue Enqueuer
	now   func() time.Time
}

func NewService(repo Repository, queue Enqueuer) *Service {
	return &Service{repo: repo, queue: queue, now: func() time.Time { return time.Now().UTC() }}
}

// Plan returns an enabled approval policy only when a transfer meets its
// per-asset threshold. No policy means the existing transfer flow is unchanged.
func (s *Service) Plan(ctx context.Context, tenantID, asset string, amount decimal.Decimal) (*domain.TransferApprovalPolicy, error) {
	if tenantID == "" {
		return nil, nil
	}
	policy, err := s.repo.GetPolicy(ctx, tenantID, strings.ToUpper(strings.TrimSpace(asset)))
	if err != nil {
		return nil, err
	}
	if policy == nil || !policy.Enabled || amount.LessThan(policy.Threshold) {
		return nil, nil
	}
	return policy, nil
}

func (s *Service) CreateRequest(ctx context.Context, tx *domain.Transaction, creatorID string, policy *domain.TransferApprovalPolicy) error {
	if policy == nil || tx == nil || tx.TenantID == nil {
		return errors.New("approval request requires a tenant-scoped transaction and policy")
	}
	expiresAfter := policy.ExpiresAfter
	if expiresAfter <= 0 {
		expiresAfter = 24 * time.Hour
	}
	request := &domain.TransferApprovalRequest{
		ID: uuid.NewString(), TenantID: *tx.TenantID, TransactionID: tx.ID, CreatorID: creatorID,
		Asset: tx.Asset, Amount: tx.Amount, RequiredApprovals: policy.RequiredApprovals,
		Status: "pending", ExpiresAt: s.now().Add(expiresAfter), CreatedAt: s.now(),
	}
	if err := s.repo.CreateRequest(ctx, request); err != nil {
		return fmt.Errorf("create transfer approval request: %w", err)
	}
	return nil
}

func (s *Service) GetPolicy(ctx context.Context, asset string) (*domain.TransferApprovalPolicy, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}
	return s.repo.GetPolicy(ctx, tenantID, strings.ToUpper(strings.TrimSpace(asset)))
}

func (s *Service) PutPolicy(ctx context.Context, policy *domain.TransferApprovalPolicy) error {
	if policy == nil || strings.TrimSpace(policy.Asset) == "" || !policy.Threshold.GreaterThan(decimal.Zero) ||
		policy.RequiredApprovals < 1 || policy.RequiredApprovals > 10 ||
		policy.ExpiresAfter < time.Minute || policy.ExpiresAfter > 30*24*time.Hour {
		return ErrInvalidPolicy
	}
	policy.TenantID = tenant.IDFromContext(ctx)
	policy.Asset = strings.ToUpper(strings.TrimSpace(policy.Asset))
	if policy.TenantID == "" {
		return domain.ErrForbidden
	}
	return s.repo.PutPolicy(ctx, policy)
}

func (s *Service) List(ctx context.Context, status string, limit, offset int) ([]*domain.TransferApprovalRequest, int, error) {
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
	return s.repo.ListRequests(ctx, tenantID, status, limit, offset)
}

func (s *Service) Decide(ctx context.Context, requestID, decision, note string) (*domain.TransferApprovalRequest, error) {
	tenantID := tenant.IDFromContext(ctx)
	actorID := tenant.UserIDFromContext(ctx)
	if tenantID == "" || actorID == "" {
		return nil, domain.ErrForbidden
	}
	if decision != "approved" && decision != "rejected" {
		return nil, ErrInvalidPolicy
	}
	request, release, err := s.repo.CastVote(ctx, tenantID, requestID, actorID, decision, note)
	if err != nil {
		return nil, err
	}
	if release && s.queue != nil {
		if err := s.queue.EnqueueTransfer(ctx, request.TransactionID); err != nil {
			return request, fmt.Errorf("approval recorded; settlement enqueue will be recovered: %w", err)
		}
	}
	return request, nil
}
