package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

type retryRepo struct {
	mockRepo
	err   error
	calls int
}

func (r *retryRepo) RetryFailedTransaction(context.Context, string) error {
	r.calls++
	return r.err
}

type retryQueue struct {
	ids []string
	err error
}

func (q *retryQueue) EnqueueTransfer(_ context.Context, id string) error {
	q.ids = append(q.ids, id)
	return q.err
}

func (*retryQueue) Enqueue(context.Context, string, interface{}) error { return nil }

func retryService(repo Repository, q taskQueue) *Service {
	s := NewService(repo, nil, nil, nil, alerting.NewClient("", "test"), nil, nil,
		"test", decimal.Zero, nil, "")
	s.queue = q
	return s
}

func TestEnqueueForceSettleRetriesFailedTransfer(t *testing.T) {
	repo := &retryRepo{}
	q := &retryQueue{}
	if err := retryService(repo, q).EnqueueForceSettle(context.Background(), "transfer-id", "operator-id"); err != nil {
		t.Fatalf("EnqueueForceSettle: %v", err)
	}
	if repo.calls != 1 || len(q.ids) != 1 || q.ids[0] != "transfer-id" {
		t.Fatalf("retry calls=%d queued IDs=%v", repo.calls, q.ids)
	}
}

func TestEnqueueForceSettleDoesNotQueueIneligibleTransfer(t *testing.T) {
	repo := &retryRepo{err: domain.ErrConcurrentUpdate}
	q := &retryQueue{}
	err := retryService(repo, q).EnqueueForceSettle(context.Background(), "transfer-id", "operator-id")
	if !errors.Is(err, domain.ErrConcurrentUpdate) {
		t.Fatalf("error=%v, want concurrent update", err)
	}
	if len(q.ids) != 0 {
		t.Fatalf("queued IDs=%v, want none", q.ids)
	}
}

func TestEnqueueForceSettleReportsQueueFailure(t *testing.T) {
	repo := &retryRepo{}
	q := &retryQueue{err: errors.New("queue unavailable")}
	err := retryService(repo, q).EnqueueForceSettle(context.Background(), "transfer-id", "operator-id")
	if err == nil {
		t.Fatal("expected queue failure")
	}
	if repo.calls != 1 || len(q.ids) != 1 {
		t.Fatalf("retry calls=%d queued IDs=%v", repo.calls, q.ids)
	}
}
