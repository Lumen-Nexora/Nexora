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
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

var (
	ErrInvalidThreshold = errors.New("threshold must be a positive number")
	ErrInvalidAsset     = errors.New("asset code is required")
)

type AuditLogger interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
}

type WebhookDispatcher interface {
	Dispatch(ctx context.Context, eventType string, payload interface{}) error
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
	GetCooldown(ctx context.Context, alertID, walletID, assetCode, assetIssuer string) (lastTriggeredAt time.Time, cooldownWindow int, exists bool, err error)
	UpsertCooldown(ctx context.Context, alertID, walletID, assetCode, assetIssuer string, lastTriggeredAt time.Time, cooldownWindow int) error
}

type Service interface {
	Create(ctx context.Context, walletID, assetCode, assetIssuer, threshold string) (*domain.WalletBalanceAlert, error)
	Get(ctx context.Context, id string) (*domain.WalletBalanceAlert, error)
	List(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error)
	Update(ctx context.Context, id, threshold string, status domain.WalletBalanceAlertStatus) (*domain.WalletBalanceAlert, error)
	Delete(ctx context.Context, id string) error
	ListEvents(ctx context.Context, alertID string, limit int) ([]*domain.WalletBalanceAlertEvent, error)
	EvaluateWalletBalances(ctx context.Context, walletID string, balances map[string]decimal.Decimal) error
}

type service struct {
	repo       Repository
	audit      AuditLogger
	webhook    WebhookDispatcher
	now        func() time.Time
	cooldownSec int
}

func NewService(repo Repository, audit AuditLogger) Service {
	return &service{repo: repo, audit: audit, webhook: nil, now: func() time.Time { return time.Now().UTC() }, cooldownSec: 3600}
}

func NewServiceWithWebhook(repo Repository, audit AuditLogger, webhook WebhookDispatcher, cooldownSec int) Service {
	if cooldownSec <= 0 {
		cooldownSec = 3600
	}
	return &service{repo: repo, audit: audit, webhook: webhook, now: func() time.Time { return time.Now().UTC() }, cooldownSec: cooldownSec}
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

func (s *service) EvaluateWalletBalances(ctx context.Context, walletID string, balances map[string]decimal.Decimal) error {
	alerts, err := s.repo.GetActiveAlertsForWallet(ctx, walletID)
	if err != nil {
		return fmt.Errorf("get active alerts for wallet: %w", err)
	}

	now := s.now()
	for _, alert := range alerts {
		balance, exists := balances[alert.AssetCode]
		if !exists {
			continue
		}

		threshold, err := decimal.NewFromString(alert.Threshold)
		if err != nil {
			continue
		}

		if balance.GreaterThanOrEqual(threshold) {
			continue
		}

		lastTriggered, cooldownWindow, cooldownExists, err := s.repo.GetCooldown(ctx, alert.ID, walletID, alert.AssetCode, alert.AssetIssuer)
		if err != nil {
			log.Warn().Err(err).Str("alert_id", alert.ID).Msg("failed to get cooldown, skipping alert")
			continue
		}

		if cooldownExists {
			timeSinceLastTrigger := now.Sub(lastTriggered)
			if timeSinceLastTrigger < time.Duration(cooldownWindow)*time.Second {
				continue
			}
		}

		event := &domain.WalletBalanceAlertEvent{
			ID:          uuid.NewString(),
			AlertID:     alert.ID,
			WalletID:    walletID,
			AssetCode:   alert.AssetCode,
			AssetIssuer: alert.AssetIssuer,
			Balance:     balance.StringFixed(7),
			Threshold:   alert.Threshold,
			TriggeredAt: now,
		}

		if err := s.repo.CreateEvent(ctx, event); err != nil {
			log.Error().Err(err).Str("alert_id", alert.ID).Msg("failed to create alert event")
			continue
		}

		if err := s.repo.UpsertCooldown(ctx, alert.ID, walletID, alert.AssetCode, alert.AssetIssuer, now, s.cooldownSec); err != nil {
			log.Error().Err(err).Str("alert_id", alert.ID).Msg("failed to upsert cooldown")
		}

		if s.webhook != nil {
			payload := map[string]interface{}{
				"alert_id":      alert.ID,
				"wallet_id":     walletID,
				"asset_code":    alert.AssetCode,
				"asset_issuer":  alert.AssetIssuer,
				"balance":       balance.StringFixed(7),
				"threshold":     alert.Threshold,
				"triggered_at":  now.Format(time.RFC3339),
			}
			if err := s.webhook.Dispatch(ctx, domain.EventWalletBalanceLow, payload); err != nil {
				log.Error().Err(err).Str("alert_id", alert.ID).Msg("failed to dispatch webhook")
			}
		}

		s.log(ctx, "wallet_balance_alert.triggered", alert.ID, map[string]interface{}{
			"wallet_id":    walletID,
			"asset_code":   alert.AssetCode,
			"balance":      balance.StringFixed(7),
			"threshold":    alert.Threshold,
		})
	}

	return nil
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
