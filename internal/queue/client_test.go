package queue

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
)

func TestClientEnqueues(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer s.Close()

	client := NewClientWithOptions(asynq.RedisClientOpt{Addr: s.Addr()})
	defer client.Close()

	ctx := context.Background()

	if err := client.EnqueueTransfer(ctx, "tx-123"); err != nil {
		t.Fatalf("EnqueueTransfer: %v", err)
	}

	if _, err := client.EnqueueWebhookDelivery(ctx, "del-123"); err != nil {
		t.Fatalf("EnqueueWebhookDelivery: %v", err)
	}

	if err := client.EnqueueTenantWebhookDelivery(ctx, "del-123", "tenant-123"); err != nil {
		t.Fatalf("EnqueueTenantWebhookDelivery: %v", err)
	}
}
