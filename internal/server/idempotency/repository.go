package idempotency

import (
	"context"
	"net/http"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

const (
	StatusProcessing = "processing"
	StatusComplete   = "complete"
)

// AcquisitionState describes what happened when a key was looked up.
type AcquisitionState uint8

const (
	// Acquired means the caller owns the processing lease and may run the
	// downstream handler.
	Acquired AcquisitionState = iota
	// Replay means a completed response is available for the exact request.
	Replay
	// InProgress means another request currently owns an unexpired lease.
	InProgress
	// LeaseExpired means the previous owner stopped without completing the
	// request. Only operations that are safe to retry may take this lease.
	LeaseExpired
	// BodyMismatch means the same key was previously used for another request.
	BodyMismatch
)

// Record is a persisted idempotency request and, once complete, its exact
// response. ID and LeaseToken fence stale owners from overwriting a recovered
// record.
type Record = domain.IdempotencyRecord

// Acquisition is the result of atomically acquiring an idempotency key.
type Acquisition struct {
	State  AcquisitionState
	Record Record
}

// Response is the durable HTTP response returned by the original request.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// LookupResult is returned by Repository.Lookup.
type LookupResult struct {
	// Found is true when a non-expired record exists for the (orgID, mode, key)
	// triple. When false, all other fields are zero-valued.
	Found  bool
	Record Record
}

// Repository persists idempotency records scoped by (orgID, mode, key).
type Repository interface {
	// Acquire atomically inserts a processing record, replays a completed
	// response, reports an active request, or takes over an expired lease.
	Acquire(ctx context.Context, orgID string, mode domain.Mode, key, requestHash string, now, leaseExpiresAt, recordExpiresAt time.Time, allowRecovery bool) (Acquisition, error)
	// Complete stores the exact response and releases the processing lease.
	// It must fail if recordID/leaseToken no longer own the processing record.
	Complete(ctx context.Context, recordID, leaseToken string, response Response, recordExpiresAt time.Time) error
	// DeleteExpired removes up to batchSize records whose retention window has
	// elapsed. It is called by the background cleanup job and is intentionally
	// batched to avoid long-held locks; the caller controls batch size.
	// Returns the number of rows deleted.
	DeleteExpired(ctx context.Context, batchSize int) (int64, error)
}

type LookupRepository interface {
	Lookup(ctx context.Context, orgID string, mode domain.Mode, key string) (LookupResult, error)
}
