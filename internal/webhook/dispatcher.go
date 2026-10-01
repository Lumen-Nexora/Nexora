package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fluxa/fluxa/internal/domain"
	"github.com/fluxa/fluxa/internal/queue"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Dispatcher handles synchronous dispatching converted to asynchronous queueing
// with SSRF-protected HTTP clients.
type Dispatcher struct {
	svc         Service
	queueClient *queue.Client
	client      *http.Client
}

// NewDispatcher creates a new Webhook Dispatcher.
func NewDispatcher(svc Service, queueClient *queue.Client, client *http.Client) *Dispatcher {
	return &Dispatcher{
		svc:         svc,
		queueClient: queueClient,
		client:      client,
	}
}

// Dispatch enqueues a webhook delivery asynchronously, removing it from the caller's request path.
func (d *Dispatcher) Dispatch(ctx context.Context, eventType string, payload interface{}) error {
	// Find subscriptions matching this event type
	subs, err := d.svc.(*service).repo.GetSubscriptionsForEvent(ctx, nil, eventType)
	if err != nil {
		return fmt.Errorf("get subscriptions for event %q: %w", eventType, err)
	}
	if len(subs) == 0 {
		return nil
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	now := time.Now().UTC()
	for _, sub := range subs {
		ep, err := d.svc.(*service).repo.GetEndpoint(ctx, sub.EndpointID)
		if err != nil {
			log.Error().Err(err).Str("endpoint_id", sub.EndpointID).Msg("webhook: failed to fetch endpoint for subscription")
			continue
		}

		if ep.Paused {
			continue
		}

		// Pre-validate the URL via SSRF guard before queueing
		if err := d.svc.(*service).validateWebhookURL(ctx, ep.URL); err != nil {
			log.Error().Err(err).Str("endpoint_url", ep.URL).Msg("webhook: skipping delivery to unsafe URL")
			continue
		}

		delivery := &domain.WebhookDelivery{
			ID:         uuid.New().String(),
			EndpointID: ep.ID,
			EventType:  eventType,
			Payload:    string(payloadBytes),
			Status:     domain.DeliveryStatusPending,
			Attempts:   0,
			CreatedAt:  now,
		}

		if err := d.svc.(*service).repo.CreateDelivery(ctx, delivery); err != nil {
			log.Error().Err(err).Str("delivery_id", delivery.ID).Msg("webhook: failed to create delivery record")
			continue
		}

		if err := d.queueClient.EnqueueWebhookDelivery(ctx, delivery.ID); err != nil {
			log.Error().Err(err).Str(
				"delivery_id", delivery.ID,
			).Msg("webhook: failed to enqueue delivery")
		}
	}

	return nil
}
