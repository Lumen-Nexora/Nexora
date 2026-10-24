package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/queue"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Dispatcher handles synchronous dispatching converted to asynchronous queueing
// with SSRF-protected HTTP clients. It satisfies indexer.EventDispatcher.
type Dispatcher struct {
	svc         *service
	queueClient *queue.Client
}

// NewDispatcher creates a new Webhook Dispatcher. svc must be the value
// returned by NewService.
func NewDispatcher(svc Service, queueClient *queue.Client) *Dispatcher {
	return &Dispatcher{
		svc:         svc.(*service),
		queueClient: queueClient,
	}
}

// Dispatch enqueues a webhook delivery asynchronously, removing it from the caller's request path.
// Endpoints whose URL fails the SSRF guard are skipped before a delivery is
// recorded, so an unsafe endpoint never reaches the delivery worker.
func (d *Dispatcher) Dispatch(ctx context.Context, eventType string, payload interface{}) error {
	var tenantPtr *string
	if tid := tenant.IDFromContext(ctx); tid != "" {
		tenantPtr = &tid
	}

	endpoints, err := d.svc.repo.ListEndpoints(ctx, tenantPtr)
	if err != nil {
		return fmt.Errorf("list endpoints for event %q: %w", eventType, err)
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	now := time.Now().UTC()
	for _, ep := range endpoints {
		if !ep.Active || !subscribesTo(ep, eventType) {
			continue
		}

		// Pre-validate the URL via SSRF guard before queueing
		if err := d.svc.validateWebhookURL(ctx, ep.URL); err != nil {
			log.Error().Err(err).Str("endpoint_url", ep.URL).Msg("webhook: skipping delivery to unsafe URL")
			continue
		}

		delivery := &domain.WebhookDelivery{
			ID:           uuid.New().String(),
			EndpointID:   ep.ID,
			TenantID:     ep.TenantID,
			EventType:    eventType,
			Payload:      string(payloadBytes),
			Status:       "pending",
			AttemptCount: 0,
			MaxAttempts:  d.svc.maxAttempts,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		if err := d.svc.repo.CreateDelivery(ctx, delivery); err != nil {
			log.Error().Err(err).Str("delivery_id", delivery.ID).Msg("webhook: failed to create delivery record")
			continue
		}

		if d.queueClient == nil {
			continue
		}
		if _, err := d.queueClient.EnqueueWebhookDelivery(ctx, delivery.ID); err != nil {
			log.Error().Err(err).Str(
				"delivery_id", delivery.ID,
			).Msg("webhook: failed to enqueue delivery")
		}
	}

	return nil
}

// subscribesTo reports whether ep listens for eventType, directly or through
// the "*" wildcard.
func subscribesTo(ep *domain.WebhookEndpoint, eventType string) bool {
	for _, ev := range ep.Events {
		if ev == eventType || ev == "*" {
			return true
		}
	}
	return false
}
