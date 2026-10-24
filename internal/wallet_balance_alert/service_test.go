package wallet_balance_alert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type mockRepo struct {
	alerts          map[string]*domain.WalletBalanceAlert
	events          map[string][]*domain.WalletBalanceAlertEvent
	cooldowns       map[string]cooldownRecord
	createErr       error
	getErr          error
}

type cooldownRecord struct {
	lastTriggeredAt time.Time
	cooldownWindow  int
}

func (m *mockRepo) Create(ctx context.Context, alert *domain.WalletBalanceAlert) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.alerts[alert.ID] = alert
	return nil
}

func (m *mockRepo) Get(ctx context.Context, id string) (*domain.WalletBalanceAlert, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.alerts[id], nil
}

func (m *mockRepo) List(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error) {
	var result []*domain.WalletBalanceAlert
	for _, a := range m.alerts {
		if walletID == "" || a.WalletID == walletID {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockRepo) Update(ctx context.Context, alert *domain.WalletBalanceAlert) error {
	m.alerts[alert.ID] = alert
	return nil
}

func (m *mockRepo) Delete(ctx context.Context, id string) error {
	delete(m.alerts, id)
	return nil
}

func (m *mockRepo) CreateEvent(ctx context.Context, event *domain.WalletBalanceAlertEvent) error {
	m.events[event.AlertID] = append(m.events[event.AlertID], event)
	return nil
}

func (m *mockRepo) ListEvents(ctx context.Context, alertID string, limit int) ([]*domain.WalletBalanceAlertEvent, error) {
	return m.events[alertID], nil
}

func (m *mockRepo) GetActiveAlertsForWallet(ctx context.Context, walletID string) ([]*domain.WalletBalanceAlert, error) {
	var result []*domain.WalletBalanceAlert
	for _, a := range m.alerts {
		if a.WalletID == walletID && a.Status == domain.WalletBalanceAlertActive {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockRepo) GetCooldown(ctx context.Context, alertID, walletID, assetCode, assetIssuer string) (time.Time, int, bool, error) {
	key := alertID + ":" + walletID + ":" + assetCode + ":" + assetIssuer
	if cd, ok := m.cooldowns[key]; ok {
		return cd.lastTriggeredAt, cd.cooldownWindow, true, nil
	}
	return time.Time{}, 0, false, nil
}

func (m *mockRepo) UpsertCooldown(ctx context.Context, alertID, walletID, assetCode, assetIssuer string, lastTriggeredAt time.Time, cooldownWindow int) error {
	key := alertID + ":" + walletID + ":" + assetCode + ":" + assetIssuer
	m.cooldowns[key] = cooldownRecord{
		lastTriggeredAt: lastTriggeredAt,
		cooldownWindow:  cooldownWindow,
	}
	return nil
}

type mockAudit struct{}

func (m *mockAudit) Record(ctx context.Context, event *domain.AuditEvent) error {
	return nil
}

func TestService_Create(t *testing.T) {
	repo := &mockRepo{alerts: make(map[string]*domain.WalletBalanceAlert), events: make(map[string][]*domain.WalletBalanceAlertEvent), cooldowns: make(map[string]cooldownRecord)}
	audit := &mockAudit{}
	svc := NewService(repo, audit)

	t.Run("valid alert creation", func(t *testing.T) {
		// Set tenant context
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, err := svc.Create(ctx, "wallet-123", "USDC", "issuer-123", "100.00")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if alert == nil {
			t.Fatal("expected alert to be created")
		}
		if alert.AssetCode != "USDC" {
			t.Errorf("expected asset code USDC, got %s", alert.AssetCode)
		}
		if alert.Threshold != "100.00" {
			t.Errorf("expected threshold 100.00, got %s", alert.Threshold)
		}
		if alert.Status != domain.WalletBalanceAlertActive {
			t.Errorf("expected status active, got %s", alert.Status)
		}
	})

	t.Run("invalid asset code", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		_, err := svc.Create(ctx, "wallet-123", "", "issuer-123", "100.00")
		if !errors.Is(err, ErrInvalidAsset) {
			t.Errorf("expected ErrInvalidAsset, got %v", err)
		}
	})

	t.Run("invalid threshold", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		_, err := svc.Create(ctx, "wallet-123", "USDC", "issuer-123", "-10.00")
		if !errors.Is(err, ErrInvalidThreshold) {
			t.Errorf("expected ErrInvalidThreshold, got %v", err)
		}
	})

	t.Run("duplicate alert", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		repo.createErr = domain.ErrDuplicateAlert
		_, err := svc.Create(ctx, "wallet-123", "USDC", "issuer-123", "100.00")
		if !errors.Is(err, domain.ErrDuplicateAlert) {
			t.Errorf("expected ErrDuplicateAlert, got %v", err)
		}
		repo.createErr = nil
	})
}

func TestService_Update(t *testing.T) {
	repo := &mockRepo{alerts: make(map[string]*domain.WalletBalanceAlert), events: make(map[string][]*domain.WalletBalanceAlertEvent), cooldowns: make(map[string]cooldownRecord)}
	audit := &mockAudit{}
	svc := NewService(repo, audit)

	t.Run("update threshold", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		// Create an alert first
		alert, _ := svc.Create(ctx, "wallet-123", "USDC", "issuer-123", "100.00")

		// Update threshold
		updated, err := svc.Update(ctx, alert.ID, "200.00", "")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if updated.Threshold != "200.00" {
			t.Errorf("expected threshold 200.00, got %s", updated.Threshold)
		}
	})

	t.Run("update status", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, _ := svc.Create(ctx, "wallet-456", "XLM", "", "50.00")

		updated, err := svc.Update(ctx, alert.ID, "", domain.WalletBalanceAlertInactive)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if updated.Status != domain.WalletBalanceAlertInactive {
			t.Errorf("expected status inactive, got %s", updated.Status)
		}
	})

	t.Run("invalid threshold", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, _ := svc.Create(ctx, "wallet-789", "EURC", "issuer-456", "100.00")

		_, err := svc.Update(ctx, alert.ID, "-5.00", "")
		if !errors.Is(err, ErrInvalidThreshold) {
			t.Errorf("expected ErrInvalidThreshold, got %v", err)
		}
	})
}

type mockWebhook struct {
	dispatchedEvents []string
	dispatchedPayloads []map[string]interface{}
}

func (m *mockWebhook) Dispatch(ctx context.Context, eventType string, payload interface{}) error {
	m.dispatchedEvents = append(m.dispatchedEvents, eventType)
	m.dispatchedPayloads = append(m.dispatchedPayloads, payload.(map[string]interface{}))
	return nil
}

func TestService_EvaluateWalletBalances(t *testing.T) {
	repo := &mockRepo{alerts: make(map[string]*domain.WalletBalanceAlert), events: make(map[string][]*domain.WalletBalanceAlertEvent), cooldowns: make(map[string]cooldownRecord)}
	audit := &mockAudit{}
	webhook := &mockWebhook{}
	svc := NewServiceWithWebhook(repo, audit, webhook, 3600)

	t.Run("triggers alert when balance below threshold", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, _ := svc.Create(ctx, "wallet-123", "USDC", "issuer-123", "100.00")

		balances := map[string]decimal.Decimal{
			"USDC": decimal.RequireFromString("50.00"),
		}

		err := svc.EvaluateWalletBalances(ctx, "wallet-123", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		events := repo.events[alert.ID]
		if len(events) != 1 {
			t.Errorf("expected 1 event, got %d", len(events))
		}

		if len(webhook.dispatchedEvents) != 1 {
			t.Errorf("expected 1 webhook dispatch, got %d", len(webhook.dispatchedEvents))
		}

		if webhook.dispatchedEvents[0] != domain.EventWalletBalanceLow {
			t.Errorf("expected event %s, got %s", domain.EventWalletBalanceLow, webhook.dispatchedEvents[0])
		}
	})

	t.Run("does not trigger when balance above threshold", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, _ := svc.Create(ctx, "wallet-456", "XLM", "", "50.00")

		balances := map[string]decimal.Decimal{
			"XLM": decimal.RequireFromString("100.00"),
		}

		err := svc.EvaluateWalletBalances(ctx, "wallet-456", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		events := repo.events[alert.ID]
		if len(events) != 0 {
			t.Errorf("expected 0 events, got %d", len(events))
		}
	})

	t.Run("respects cooldown window", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		_, _ = svc.Create(ctx, "wallet-789", "EURC", "issuer-456", "100.00")

		balances := map[string]decimal.Decimal{
			"EURC": decimal.RequireFromString("50.00"),
		}

		webhook.dispatchedEvents = nil
		webhook.dispatchedPayloads = nil

		err := svc.EvaluateWalletBalances(ctx, "wallet-789", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		if len(webhook.dispatchedEvents) != 1 {
			t.Errorf("expected 1 webhook dispatch on first trigger, got %d", len(webhook.dispatchedEvents))
		}

		err = svc.EvaluateWalletBalances(ctx, "wallet-789", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		if len(webhook.dispatchedEvents) != 1 {
			t.Errorf("expected no additional webhook dispatch during cooldown, got %d", len(webhook.dispatchedEvents))
		}
	})

	t.Run("tenant isolation - only checks tenant alerts", func(t *testing.T) {
		tenant1ID := uuid.NewString()
		tenant2ID := uuid.NewString()

		ctx1 := context.Background()
		ctx1 = context.WithValue(ctx1, "tenant_id", tenant1ID)
		ctx1 = context.WithValue(ctx1, "mode", domain.ModeLive)

		ctx2 := context.Background()
		ctx2 = context.WithValue(ctx2, "tenant_id", tenant2ID)
		ctx2 = context.WithValue(ctx2, "mode", domain.ModeLive)

		alert1, _ := svc.Create(ctx1, "wallet-tenant1", "USDC", "issuer-123", "100.00")
		_, _ = svc.Create(ctx2, "wallet-tenant2", "USDC", "issuer-123", "100.00")

		balances := map[string]decimal.Decimal{
			"USDC": decimal.RequireFromString("50.00"),
		}

		webhook.dispatchedEvents = nil

		err := svc.EvaluateWalletBalances(ctx1, "wallet-tenant1", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		events1 := repo.events[alert1.ID]
		if len(events1) != 1 {
			t.Errorf("expected 1 event for tenant1, got %d", len(events1))
		}

		if len(webhook.dispatchedEvents) != 1 {
			t.Errorf("expected 1 webhook dispatch for tenant1, got %d", len(webhook.dispatchedEvents))
		}
	})

	t.Run("threshold boundary condition - equal to threshold", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, _ := svc.Create(ctx, "wallet-boundary", "USDC", "issuer-123", "100.00")

		balances := map[string]decimal.Decimal{
			"USDC": decimal.RequireFromString("100.00"),
		}

		webhook.dispatchedEvents = nil

		err := svc.EvaluateWalletBalances(ctx, "wallet-boundary", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		events := repo.events[alert.ID]
		if len(events) != 0 {
			t.Errorf("expected 0 events when balance equals threshold, got %d", len(events))
		}
	})

	t.Run("threshold boundary condition - one stroop below", func(t *testing.T) {
		ctx := context.Background()
		ctx = context.WithValue(ctx, "tenant_id", uuid.NewString())
		ctx = context.WithValue(ctx, "mode", domain.ModeLive)

		alert, _ := svc.Create(ctx, "wallet-boundary2", "USDC", "issuer-123", "100.00")

		balances := map[string]decimal.Decimal{
			"USDC": decimal.RequireFromString("99.9999999"),
		}

		webhook.dispatchedEvents = nil

		err := svc.EvaluateWalletBalances(ctx, "wallet-boundary2", balances)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		events := repo.events[alert.ID]
		if len(events) != 1 {
			t.Errorf("expected 1 event when balance is below threshold, got %d", len(events))
		}
	})
}
