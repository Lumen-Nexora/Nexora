package apikey

import (
	"context"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/rs/zerolog/log"
)

type WebhookDispatcher interface {
	Dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) error
}

type ExpiryNotifier interface {
	Send(ctx context.Context, level string, title, message string)
}

// Minimal interface needed by ExpiryTracker to decouple or use concrete repo.
type KeyRepository interface {
	ListExpiringKeys(ctx context.Context, limit int) ([]*domain.APIKey, error)
	RecordRotationReminder(ctx context.Context, id string, at time.Time) error
}

type Tracker struct {
	repo       KeyRepository
	webhooks   WebhookDispatcher
	alerts     ExpiryNotifier
	pollPeriod time.Duration
	stopCh     chan struct{}
}

func NewTracker(repo KeyRepository, webhooks WebhookDispatcher, alerts ExpiryNotifier, pollPeriod time.Duration) *Tracker {
	if pollPeriod <= 0 {
		pollPeriod = 1 * time.Hour
	}
	return &Tracker{
		repo:       repo,
		webhooks:   webhooks,
		alerts:     alerts,
		pollPeriod: pollPeriod,
		stopCh:     make(chan struct{}),
	}
}

func (t *Tracker) Start(ctx context.Context) {
	ticker := time.NewTicker(t.pollPeriod)
	go func() {
		defer ticker.Stop()
		// Run initial check on startup
		t.CheckExpiringKeys(ctx, time.Now().UTC())

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.stopCh:
				return
			case now := <-ticker.C:
				t.CheckExpiringKeys(ctx, now.UTC())
			}
		}
	}()
}

func (t *Tracker) Stop() {
	close(t.stopCh)
}

// CheckExpiringKeys inspects expiring keys and dispatches rotation reminders or expiration events.
func (t *Tracker) CheckExpiringKeys(ctx context.Context, now time.Time) int {
	keys, err := t.repo.ListExpiringKeys(ctx, 200)
	if err != nil {
		log.Error().Err(err).Msg("expiry_tracker: failed to list expiring keys")
		return 0
	}

	notified := 0
	for _, key := range keys {
		if key.NeedsRotationReminder(now) {
			t.notifyRotationReminder(ctx, key, now)
			if err := t.repo.RecordRotationReminder(ctx, key.ID, now); err != nil {
				log.Error().Err(err).Str("key_id", key.ID).Msg("expiry_tracker: record reminder timestamp")
			}
			notified++
		}
	}
	return notified
}

func (t *Tracker) notifyRotationReminder(ctx context.Context, key *domain.APIKey, now time.Time) {
	daysLeft := int(key.ExpiresAt.Sub(now).Hours() / 24)
	if daysLeft < 0 {
		daysLeft = 0
	}

	payload := map[string]interface{}{
		"key_id":                 key.ID,
		"tenant_id":              key.TenantID,
		"prefix":                 key.Prefix,
		"label":                  key.Label,
		"mode":                   key.Mode,
		"role":                   key.Role,
		"expires_at":             key.ExpiresAt.Format(time.RFC3339),
		"days_until_expiration":  daysLeft,
		"rotation_reminder_days": key.RotationReminderDays,
		"reminded_at":            now.Format(time.RFC3339),
	}

	if t.webhooks != nil {
		if err := t.webhooks.Dispatch(ctx, domain.EventAPIKeyRotationReminder, payload); err != nil {
			log.Warn().Err(err).Str("key_id", key.ID).Msg("expiry_tracker: dispatch rotation reminder webhook")
		}
	}

	if t.alerts != nil {
		t.alerts.Send(ctx, "WARNING", "API Key Rotation Required",
			"API key "+key.Prefix+"... for tenant "+key.TenantID+" expires in "+time.Duration(daysLeft*24*int(time.Hour)).String()+". Please rotate credentials.")
	}

	log.Info().
		Str("key_id", key.ID).
		Str("tenant_id", key.TenantID).
		Str("prefix", key.Prefix).
		Int("days_until_expiration", daysLeft).
		Msg("api_key: rotation reminder dispatched")
}
