package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/server/idempotency"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrIdempotencyLeaseLost = errors.New("idempotency processing lease is no longer owned")

const (
	StatusProcessing = "processing"
	StatusComplete   = "complete"
)

// AcquisitionState describes what happened when a key was looked up.
type AcquisitionState = idempotency.AcquisitionState

const (
	Acquired     = idempotency.Acquired
	Replay       = idempotency.Replay
	InProgress   = idempotency.InProgress
	LeaseExpired = idempotency.LeaseExpired
	BodyMismatch = idempotency.BodyMismatch
)

// Acquisition is the result of atomically acquiring an idempotency key.
type Acquisition = idempotency.Acquisition

// Response is the durable HTTP response returned by the original request.
type Response = idempotency.Response

type IdempotencyRepo struct {
	db DB
}

func NewIdempotencyRepo(db DB) *IdempotencyRepo {
	return &IdempotencyRepo{db: db}
}

// Acquire atomically claims (orgID, mode, key) for a new request. A record that
// has outlived its retention window is replaced with a fresh generation so a
// recovered request can never be confused with a later one that reuses the key.
func (r *IdempotencyRepo) Acquire(ctx context.Context, orgID string, mode domain.Mode, key, requestHash string, now, leaseExpiresAt, recordExpiresAt time.Time, allowRecovery bool) (Acquisition, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return idempotency.Acquisition{}, fmt.Errorf("begin idempotency acquisition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	acquisition, inserted, err := insertProcessingRecord(ctx, tx, orgID, mode, key, requestHash, leaseExpiresAt, recordExpiresAt)
	if err != nil {
		return idempotency.Acquisition{}, err
	}
	if inserted {
		if err := tx.Commit(ctx); err != nil {
			return idempotency.Acquisition{}, fmt.Errorf("commit idempotency acquisition: %w", err)
		}
		return acquisition, nil
	}

	rec, err := lockExistingRecord(ctx, tx, orgID, mode, key)
	if err != nil {
		return idempotency.Acquisition{}, err
	}

	// A fully retained generation gets a new record ID. Transactions linked to
	// an expired generation therefore cannot be mistaken for a later request
	// that happens to reuse the same client key.
	if !rec.ExpiresAt.After(now.UTC()) {
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_records WHERE id = $1`, rec.ID); err != nil {
			return idempotency.Acquisition{}, fmt.Errorf("delete expired idempotency record: %w", err)
		}
		acquisition, _, err = insertProcessingRecord(ctx, tx, orgID, mode, key, requestHash, leaseExpiresAt, recordExpiresAt)
		if err != nil {
			return idempotency.Acquisition{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return idempotency.Acquisition{}, fmt.Errorf("commit idempotency generation rollover: %w", err)
		}
		return acquisition, nil
	}

	// A request that is still (or was never) finished owns the key. Its body is
	// irrelevant: the client must retry later rather than start a competing
	// generation, so the body-mismatch check runs only once the original has
	// completed and a response actually exists to compare against.
	now = now.UTC()
	if rec.Status == StatusProcessing {
		if rec.LeaseExpiresAt.IsZero() || !rec.LeaseExpiresAt.After(now) {
			if allowRecovery {
				newToken := uuid.New().String()
				if _, err := tx.Exec(ctx,
					`UPDATE idempotency_records
					 SET lease_token = $2, lease_expires_at = $3, updated_at = NOW()
					 WHERE id = $1 AND status = 'processing'`,
					rec.ID, newToken, leaseExpiresAt.UTC(),
				); err != nil {
					return Acquisition{}, fmt.Errorf("take over idempotency lease: %w", err)
				}
				rec.LeaseToken = newToken
				rec.LeaseExpiresAt = leaseExpiresAt.UTC()
			}
			if err := tx.Commit(ctx); err != nil {
				return Acquisition{}, fmt.Errorf("commit idempotency lease takeover: %w", err)
			}
			return Acquisition{State: LeaseExpired, Record: rec}, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return Acquisition{}, fmt.Errorf("commit idempotency in-progress lookup: %w", err)
		}
		return Acquisition{State: InProgress, Record: rec}, nil
	}

	if rec.RequestHash != requestHash {
		if err := tx.Commit(ctx); err != nil {
			return Acquisition{}, fmt.Errorf("commit idempotency body mismatch: %w", err)
		}
		return Acquisition{State: BodyMismatch, Record: rec}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return Acquisition{}, fmt.Errorf("commit idempotency replay lookup: %w", err)
	}
	return Acquisition{State: Replay, Record: rec}, nil
}

func insertProcessingRecord(ctx context.Context, tx pgx.Tx, orgID string, mode domain.Mode, key, requestHash string, leaseExpiresAt, recordExpiresAt time.Time) (Acquisition, bool, error) {
	recordID := uuid.New().String()
	leaseToken := uuid.New().String()
	var insertedID string
	err := tx.QueryRow(ctx,
		`INSERT INTO idempotency_records
			(id, org_id, mode, key, request_hash, status, lease_token, lease_expires_at, created_at, updated_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'processing', $6, $7, NOW(), NOW(), $8)
		 ON CONFLICT (org_id, mode, key) DO NOTHING
		 RETURNING id`,
		recordID, orgID, mode, key, requestHash, leaseToken, leaseExpiresAt.UTC(), recordExpiresAt.UTC(),
	).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Acquisition{}, false, nil
	}
	if err != nil {
		return Acquisition{}, false, fmt.Errorf("insert idempotency record: %w", err)
	}
	return Acquisition{
		State: Acquired,
		Record: domain.IdempotencyRecord{
			ID:             insertedID,
			OrgID:          orgID,
			Mode:           mode,
			Key:            key,
			RequestHash:    requestHash,
			Status:         "processing",
			LeaseToken:     leaseToken,
			LeaseExpiresAt: leaseExpiresAt.UTC(),
			ExpiresAt:      recordExpiresAt.UTC(),
		},
	}, true, nil
}

func lockExistingRecord(ctx context.Context, tx pgx.Tx, orgID string, mode domain.Mode, key string) (domain.IdempotencyRecord, error) {
	rec := domain.IdempotencyRecord{OrgID: orgID, Mode: mode, Key: key}
	var responseStatus *int
	var responseHeaders []byte
	var leaseToken *string
	var leaseExpiresAt *time.Time
	var recordExpiresAt time.Time
	err := tx.QueryRow(ctx,
		`SELECT id, mode, request_hash, status, response_status, response_headers,
		        response_body_bytes, lease_token, lease_expires_at, expires_at
		 FROM idempotency_records
		 WHERE org_id = $1 AND mode = $2 AND key = $3
		 FOR UPDATE`,
		orgID, mode, key,
	).Scan(
		&rec.ID,
		&rec.Mode,
		&rec.RequestHash,
		&rec.Status,
		&responseStatus,
		&responseHeaders,
		&rec.ResponseBody,
		&leaseToken,
		&leaseExpiresAt,
		&recordExpiresAt,
	)
	if err != nil {
		return domain.IdempotencyRecord{}, fmt.Errorf("lock idempotency record: %w", err)
	}
	if leaseToken != nil {
		rec.LeaseToken = *leaseToken
	}
	if leaseExpiresAt != nil {
		rec.LeaseExpiresAt = leaseExpiresAt.UTC()
	}
	rec.ExpiresAt = recordExpiresAt.UTC()
	if responseStatus != nil {
		rec.ResponseStatus = *responseStatus
	}
	if len(responseHeaders) > 0 {
		rec.ResponseHeaders = make(http.Header)
		if err := json.Unmarshal(responseHeaders, &rec.ResponseHeaders); err != nil {
			return domain.IdempotencyRecord{}, fmt.Errorf("decode idempotency response headers: %w", err)
		}
	}
	return rec, nil
}

// Complete stores the exact response and releases the processing lease. It is
// fenced on the lease token: if the lease was taken over (or the record was
// already completed) the write is rejected so a stale owner cannot overwrite
// the recovered generation's response.
func (r *IdempotencyRepo) Complete(ctx context.Context, recordID, leaseToken string, response Response, recordExpiresAt time.Time) error {
	headers, err := json.Marshal(response.Headers)
	if err != nil {
		return fmt.Errorf("encode idempotency response headers: %w", err)
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE idempotency_records
		 SET status = 'complete',
		     response_status = $3,
		     response_headers = $4,
		     response_body_bytes = $5,
		     lease_token = NULL,
		     lease_expires_at = NULL,
		     expires_at = $6,
		     updated_at = NOW()
		 WHERE id = $1
		   AND lease_token = $2
		   AND status = 'processing'`,
		recordID, leaseToken, response.Status, headers, response.Body, recordExpiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrIdempotencyLeaseLost
	}
	return nil
}

// DeleteExpired removes up to batchSize rows whose retention window has
// elapsed. Callers should loop until the returned count is zero to drain the
// backlog without issuing one enormous DELETE that could spike I/O or hold
// locks.
func (r *IdempotencyRepo) DeleteExpired(ctx context.Context, batchSize int) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM idempotency_records
		 WHERE id IN (
		     SELECT id FROM idempotency_records
		     WHERE expires_at <= NOW()
		     LIMIT $1
		 )`,
		batchSize,
	)
	if err != nil {
		return 0, fmt.Errorf("purge expired idempotency records: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Lookup returns the current state of a single idempotency record for the
// (orgID, mode, key) triple without acquiring any lease or touching any row.
// A record whose expires_at is in the past is reported as not found, matching
// the client-visible retention window even if the background cleanup job has
// not yet run.
func (r *IdempotencyRepo) Lookup(ctx context.Context, orgID string, mode domain.Mode, key string) (idempotency.LookupResult, error) {
	rec := idempotency.Record{OrgID: orgID, Mode: mode, Key: key}
	var responseStatus *int
	var responseHeaders []byte
	var leaseToken *string
	var leaseExpiresAt *time.Time
	var recordExpiresAt time.Time
	var createdAt time.Time

	err := r.db.QueryRow(ctx,
		`SELECT id, mode, request_hash, status, response_status, response_headers,
		        response_body_bytes, lease_token, lease_expires_at, expires_at, created_at
		 FROM idempotency_records
		 WHERE org_id = $1 AND mode = $2 AND key = $3
		   AND expires_at > NOW()`,
		orgID, mode, key,
	).Scan(
		&rec.ID,
		&rec.Mode,
		&rec.RequestHash,
		&rec.Status,
		&responseStatus,
		&responseHeaders,
		&rec.ResponseBody,
		&leaseToken,
		&leaseExpiresAt,
		&recordExpiresAt,
		&createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return idempotency.LookupResult{Found: false}, nil
	}
	if err != nil {
		return idempotency.LookupResult{}, fmt.Errorf("lookup idempotency record: %w", err)
	}
	rec.CreatedAt = createdAt.UTC()
	if leaseToken != nil {
		rec.LeaseToken = *leaseToken
	}
	if leaseExpiresAt != nil {
		rec.LeaseExpiresAt = leaseExpiresAt.UTC()
	}
	rec.ExpiresAt = recordExpiresAt.UTC()
	if responseStatus != nil {
		rec.ResponseStatus = *responseStatus
	}
	if len(responseHeaders) > 0 {
		rec.ResponseHeaders = make(http.Header)
		if err := json.Unmarshal(responseHeaders, &rec.ResponseHeaders); err != nil {
			return idempotency.LookupResult{}, fmt.Errorf("decode idempotency response headers for lookup: %w", err)
		}
	}
	return idempotency.LookupResult{Found: true, Record: rec}, nil
}
