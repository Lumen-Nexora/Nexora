package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// staleClaimAfter is how long a consolidation may stay claimed by a worker
// before another delivery of its task may take it over. It comfortably
// exceeds a run over the maximum number of source wallets.
const staleClaimAfter = 15 * time.Minute

// ConsolidationRepository handles persistence of wallet consolidation operations.
type ConsolidationRepository interface {
	// Create inserts a pending operation. It returns
	// ErrConsolidationKeyConflict when the tenant already used the key.
	Create(ctx context.Context, op *domain.ConsolidationOperation) error
	Get(ctx context.Context, id, tenantID string) (*domain.ConsolidationOperation, error)
	// GetByIdempotencyKey returns nil, nil when the key is unused.
	GetByIdempotencyKey(ctx context.Context, tenantID, key string) (*domain.ConsolidationOperation, error)
	List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.ConsolidationOperation, error)
	// Claim moves a pending (or stale processing) operation to processing.
	// It returns false when another worker holds it or it already finished.
	Claim(ctx context.Context, id, tenantID string) (bool, error)
	// Finish stores the final status, batches and totals.
	Finish(ctx context.Context, op *domain.ConsolidationOperation) error
}

type consolidationRepository struct {
	db ClosureDB
}

// NewConsolidationRepository creates a new consolidation repository.
func NewConsolidationRepository(db ClosureDB) ConsolidationRepository {
	return &consolidationRepository{db: db}
}

const consolidationColumns = `id, tenant_id, mode, source_wallet_ids::text[], destination_wallet::text,
	idempotency_key, merge_sources, batch_size, total_fee, reserve_recovered, status, dry_run,
	COALESCE(transaction_hashes, '{}'), batches, actor_type, actor_id, error_message,
	request_fingerprint, created_at, completed_at`

func (r *consolidationRepository) Create(ctx context.Context, op *domain.ConsolidationOperation) error {
	op.ID = uuid.New().String()
	op.CreatedAt = time.Now().UTC()
	op.Status = domain.ConsolidationPending
	if op.Batches == nil {
		op.Batches = []domain.ConsolidationBatch{}
	}
	batches, err := json.Marshal(op.Batches)
	if err != nil {
		return fmt.Errorf("encode consolidation batches: %w", err)
	}

	_, err = r.db.Exec(ctx, `
		INSERT INTO consolidation_operations
		(id, tenant_id, mode, source_wallet_ids, destination_wallet, idempotency_key,
		 merge_sources, batch_size, total_fee, reserve_recovered, status, dry_run,
		 batches, actor_type, actor_id, request_fingerprint, created_at)
		VALUES ($1, $2, $3, $4::uuid[], $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`, op.ID, op.TenantID, op.Mode, op.SourceWalletIDs, op.DestinationWallet, op.IdempotencyKey,
		op.MergeSources, op.BatchSize, op.TotalFee, op.ReserveRecovered, op.Status, op.DryRun,
		batches, op.ActorType, op.ActorID, op.Fingerprint, op.CreatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrConsolidationKeyConflict
	}
	if err != nil {
		return fmt.Errorf("insert consolidation: %w", err)
	}
	return nil
}

func (r *consolidationRepository) Get(ctx context.Context, id, tenantID string) (*domain.ConsolidationOperation, error) {
	op, err := scanConsolidation(r.db.QueryRow(ctx, `SELECT `+consolidationColumns+`
		FROM consolidation_operations
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConsolidationNotFound
	}
	return op, err
}

func (r *consolidationRepository) GetByIdempotencyKey(ctx context.Context, tenantID, key string) (*domain.ConsolidationOperation, error) {
	op, err := scanConsolidation(r.db.QueryRow(ctx, `SELECT `+consolidationColumns+`
		FROM consolidation_operations
		WHERE tenant_id = $1 AND idempotency_key = $2
	`, tenantID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return op, err
}

func (r *consolidationRepository) List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.ConsolidationOperation, error) {
	rows, err := r.db.Query(ctx, `SELECT `+consolidationColumns+`
		FROM consolidation_operations
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list consolidations: %w", err)
	}
	defer rows.Close()

	ops := []*domain.ConsolidationOperation{}
	for rows.Next() {
		op, err := scanConsolidation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

func (r *consolidationRepository) Claim(ctx context.Context, id, tenantID string) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE consolidation_operations
		SET status = $1, claimed_at = NOW()
		WHERE id = $2 AND tenant_id = $3
		  AND (status = $4 OR (status = $1 AND claimed_at < NOW() - $5::interval))
	`, domain.ConsolidationProcessing, id, tenantID, domain.ConsolidationPending,
		fmt.Sprintf("%d seconds", int(staleClaimAfter.Seconds())))
	if err != nil {
		return false, fmt.Errorf("claim consolidation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *consolidationRepository) Finish(ctx context.Context, op *domain.ConsolidationOperation) error {
	batches, err := json.Marshal(op.Batches)
	if err != nil {
		return fmt.Errorf("encode consolidation batches: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		UPDATE consolidation_operations
		SET status = $1, transaction_hashes = $2, batches = $3, total_fee = $4,
		    reserve_recovered = $5, error_message = $6, completed_at = $7
		WHERE id = $8 AND tenant_id = $9
	`, op.Status, op.TransactionHashes, batches, op.TotalFee, op.ReserveRecovered,
		op.ErrorMessage, op.CompletedAt, op.ID, op.TenantID)
	if err != nil {
		return fmt.Errorf("finish consolidation: %w", err)
	}
	return nil
}

func scanConsolidation(row pgx.Row) (*domain.ConsolidationOperation, error) {
	op := &domain.ConsolidationOperation{}
	var batches []byte
	err := row.Scan(
		&op.ID, &op.TenantID, &op.Mode, &op.SourceWalletIDs, &op.DestinationWallet,
		&op.IdempotencyKey, &op.MergeSources, &op.BatchSize, &op.TotalFee, &op.ReserveRecovered,
		&op.Status, &op.DryRun, &op.TransactionHashes, &batches, &op.ActorType, &op.ActorID,
		&op.ErrorMessage, &op.Fingerprint, &op.CreatedAt, &op.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	op.Batches = []domain.ConsolidationBatch{}
	if len(batches) > 0 {
		if err := json.Unmarshal(batches, &op.Batches); err != nil {
			return nil, fmt.Errorf("decode consolidation batches: %w", err)
		}
	}
	return op, nil
}
