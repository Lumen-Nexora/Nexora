package wallet_balance_alert

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

var (
	ErrInvalidThreshold = errors.New("threshold must be a positive number")
	ErrInvalidAsset     = errors.New("asset code is required")
)

type AuditLogger interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
}

type Repository interface {
	Create(ctx context.Context, alert *domain.WalletBalanceAlert) error
	Get(ctx context.Context, id string) (*domain.WalletBalanceAlert, error)
	List(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error)
	Update(ctx context.Context, alert *domain.WalletBalanceAlert) error
	Delete(ctx context.Context, id string) error
	CreateEvent(ctx context.Context, event *domain.WalletBalanceAlertEvent) error
	ListEvents(ctx context.Context, alertID string, limit int) ([]*domain.WalletBalanceAlertEvent, error)
	GetActiveAlertsForWallet(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error)
}

type Service interface {
	Create(ctx context.Context, walletID, assetCode, assetIssuer, threshold string) (*domain.WalletBalanceAlert, error)
	Get(ctx context.Context, id string) (*domain.WalletBalanceAlert, error)
	List(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error)
	Update(ctx context.Context, id, threshold string, status domain.WalletBalanceAlertStatus) (*domain.WalletBalanceAlert, error)
	Delete(ctx context.Context, id string) error
	ListEvents(ctx context.Context, alertID string, limit int) ([]*domain.WalletBalanceAlertEvent, error)
}

type service struct {
	repo  Repository
	audit AuditLogger
	now   func() time.Time
}

func NewService(repo Repository, audit AuditLogger) Service {
	return &service{repo: repo, audit: audit, now: func() time.Time { return time.Now().UTC() }}
}

func (s *service) Create(ctx context.Context, walletID, assetCode, assetIssuer, threshold string) (*domain.WalletBalanceAlert, error) {
	assetCode = strings.TrimSpace(assetCode)
	if assetCode == "" {
		return nil, ErrInvalidAsset
	}

	thresholdDecimal, err := decimal.NewFromString(threshold)
	if err != nil || thresholdDecimal.LessThanOrEqual(decimal.Zero) {
		return nil, ErrInvalidThreshold
	}

	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrForbidden
	}

	now := s.now()
	a := &domain.WalletBalanceAlert{
		ID:          uuid.NewString(),
		TenantID:    tenantID,
		Mode:        tenant.ModeOrDefault(ctx, domain.ModeLive),
		WalletID:    walletID,
		AssetCode:   assetCode,
		AssetIssuer: strings.TrimSpace(assetIssuer),
		Threshold:   threshold,
		Status:      domain.WalletBalanceAlertActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.repo.Create(ctx, a); err != nil {
		if errors.Is(err, domain.ErrDuplicateAlert) {
			return nil, domain.ErrDuplicateAlert
		}
		return nil, fmt.Errorf("create wallet balance alert: %w", err)
	}

	s.log(ctx, "wallet_balance_alert.created", a.ID, map[string]interface{}{
		"wallet_id":    a.WalletID,
		"asset_code":   a.AssetCode,
		"asset_issuer": a.AssetIssuer,
		"threshold":    a.Threshold,
	})

	return a, nil
}

func (s *service) Get(ctx context.Context, id string) (*domain.WalletBalanceAlert, error) {
	a, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *service) List(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error) {
	return s.repo.List(ctx, walletID)
}

func (s *service) Update(ctx context.Context, id, threshold string, status domain.WalletBalanceAlertStatus) (*domain.WalletBalanceAlert, error) {
	a, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if threshold != "" {
		thresholdDecimal, err := decimal.NewFromString(threshold)
		if err != nil || thresholdDecimal.LessThanOrEqual(decimal.Zero) {
			return nil, ErrInvalidThreshold
		}
		a.Threshold = threshold
	}

	if status != "" {
		a.Status = status
	}

	a.UpdatedAt = s.now()

	if err := s.repo.Update(ctx, a); err != nil {
		return nil, fmt.Errorf("update wallet balance alert: %w", err)
	}

	s.log(ctx, "wallet_balance_alert.updated", id, map[string]interface{}{
		"threshold": a.Threshold,
		"status":    a.Status,
	})

	return a, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}

	s.log(ctx, "wallet_balance_alert.deleted", id, nil)
	return nil
}

func (s *service) ListEvents(ctx context.Context, alertID string, limit int) ([]*domain.WalletBalanceAlertEvent, error) {
	return s.repo.ListEvents(ctx, alertID, limit)
}

func (s *service) log(ctx context.Context, action, id string, metadata map[string]interface{}) {
	if s.audit != nil {
		_ = s.audit.Record(ctx, &domain.AuditEvent{
			Action:       action,
			ResourceType: "wallet_balance_alert",
			ResourceID:   id,
			Metadata:     metadata,
		})
	}
}
