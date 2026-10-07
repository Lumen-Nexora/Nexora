package beneficiary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/stellar/go/keypair"
)

const CoolingOffPeriod = 24 * time.Hour

var (
	ErrNotFound       = errors.New("beneficiary not found")
	ErrDuplicate      = errors.New("beneficiary already exists")
	ErrCoolingOff     = errors.New("beneficiary is still in its cooling-off period")
	ErrNotActive      = errors.New("beneficiary is not active")
	ErrInvalidAccount = errors.New("beneficiary account must be a Stellar public key")
)

type AuditLogger interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
}

type Service interface {
	Create(ctx context.Context, account, label string) (*domain.Beneficiary, error)
	Get(ctx context.Context, id string) (*domain.Beneficiary, error)
	List(ctx context.Context) ([]*domain.Beneficiary, error)
	Activate(ctx context.Context, id string) (*domain.Beneficiary, error)
	Revoke(ctx context.Context, id string) error
	Check(ctx context.Context, account string) (configured, active bool, err error)
}

type service struct {
	repo  Repository
	audit AuditLogger
	now   func() time.Time
}

func NewService(repo Repository, audit AuditLogger) Service {
	return &service{repo: repo, audit: audit, now: func() time.Time { return time.Now().UTC() }}
}

func (s *service) Create(ctx context.Context, account, label string) (*domain.Beneficiary, error) {
	account = strings.TrimSpace(account)
	if _, err := keypair.ParseAddress(account); err != nil {
		return nil, ErrInvalidAccount
	}
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}
	now := s.now()
	b := &domain.Beneficiary{
		ID: uuid.NewString(), TenantID: tenantID, Mode: tenant.ModeOrDefault(ctx, domain.ModeLive),
		Account: account, Label: strings.TrimSpace(label), Status: domain.BeneficiaryPending,
		CooldownUntil: now.Add(CoolingOffPeriod), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, b); err != nil {
		if errors.Is(err, ErrDuplicate) {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("create beneficiary: %w", err)
	}
	s.log(ctx, "beneficiary.created", b.ID, map[string]interface{}{"account": b.Account, "cooldown_until": b.CooldownUntil})
	return b, nil
}

func (s *service) Get(ctx context.Context, id string) (*domain.Beneficiary, error) {
	b, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *service) List(ctx context.Context) ([]*domain.Beneficiary, error) { return s.repo.List(ctx) }

// Check returns whether this tenant has configured an allowlist and whether
// the requested account is currently active. An empty list preserves existing
// tenants' behaviour until they opt into beneficiary enforcement.
func (s *service) Check(ctx context.Context, account string) (bool, bool, error) {
	return s.repo.Check(ctx, account)
}

func (s *service) Activate(ctx context.Context, id string) (*domain.Beneficiary, error) {
	b, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if b.Status == domain.BeneficiaryRevoked {
		return nil, ErrNotActive
	}
	if now.Before(b.CooldownUntil) {
		return nil, ErrCoolingOff
	}
	if b.Status == domain.BeneficiaryActive {
		return b, nil
	}
	updated, err := s.repo.Activate(ctx, id, now)
	if err != nil {
		return nil, err
	}
	s.log(ctx, "beneficiary.activated", id, nil)
	return updated, nil
}

func (s *service) Revoke(ctx context.Context, id string) error {
	if err := s.repo.Revoke(ctx, id); err != nil {
		return err
	}
	s.log(ctx, "beneficiary.revoked", id, nil)
	return nil
}

func (s *service) log(ctx context.Context, action, id string, metadata map[string]interface{}) {
	if s.audit != nil {
		_ = s.audit.Record(ctx, &domain.AuditEvent{Action: action, ResourceType: "beneficiary", ResourceID: id, Metadata: metadata})
	}
}
