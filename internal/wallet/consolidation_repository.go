package wallet

import (
	"context"
	"database/sql"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ConsolidationRepository handles persistence of wallet consolidation operations.
type ConsolidationRepository interface {
	Create(ctx context.Context, op *domain.ConsolidationOperation) error
	Get(ctx context.Context, id, tenantID string) (*domain.ConsolidationOperation, error)
	GetByIdempotencyKey(ctx context.Context, tenantID, key string) (*domain.ConsolidationOperation, error)
	UpdateStatus(ctx context.Context, id, status string, txHashes []string, err error) error
	List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.ConsolidationOperation, error)
}

type consolidationRepository struct {
	db *sql.DB
}

// NewConsolidationRepository creates a new consolidation repository.
func NewConsolidationRepository(db *sql.DB) ConsolidationRepository {
	return &consolidationRepository{db: db}
}

func (r *consolidationRepository) Create(ctx context.Context, op *domain.ConsolidationOperation) error {
	op.ID = uuid.New().String()
	op.CreatedAt = time.Now()
	op.Status = "pending"

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO consolidation_operations 
		(id, tenant_id, source_wallet_ids, destination_wallet, idempotency_key, 
		 total_fee, reserve_recovered, status, dry_run, actor_type, actor_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, op.ID, op.TenantID, pq.Array(op.SourceWalletIDs), op.DestinationWallet,
		op.IdempotencyKey, op.TotalFee, op.ReserveRecovered, op.Status, op.DryRun,
		op.ActorType, op.ActorID, op.CreatedAt)

	return err
}

func (r *consolidationRepository) Get(ctx context.Context, id, tenantID string) (*domain.ConsolidationOperation, error) {
	op := &domain.ConsolidationOperation{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, source_wallet_ids, destination_wallet, idempotency_key,
		       total_fee, reserve_recovered, status, dry_run, transaction_hashes,
		       actor_type, actor_id, error_message, created_at, completed_at
		FROM consolidation_operations
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID).Scan(
		&op.ID, &op.TenantID, pq.Array(&op.SourceWalletIDs), &op.DestinationWallet,
		&op.IdempotencyKey, &op.TotalFee, &op.ReserveRecovered, &op.Status, &op.DryRun,
		pq.Array(&op.TransactionHashes), &op.ActorType, &op.ActorID, &op.ErrorMessage,
		&op.CreatedAt, &op.CompletedAt,
	)

	if err == sql.ErrNoRows {
		return nil, domain.ErrWalletNotFound
	}
	return op, err
}

func (r *consolidationRepository) GetByIdempotencyKey(ctx context.Context, tenantID, key string) (*domain.ConsolidationOperation, error) {
	op := &domain.ConsolidationOperation{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, source_wallet_ids, destination_wallet, idempotency_key,
		       total_fee, reserve_recovered, status, dry_run, transaction_hashes,
		       actor_type, actor_id, error_message, created_at, completed_at
		FROM consolidation_operations
		WHERE tenant_id = $1 AND idempotency_key = $2
	`, tenantID, key).Scan(
		&op.ID, &op.TenantID, pq.Array(&op.SourceWalletIDs), &op.DestinationWallet,
		&op.IdempotencyKey, &op.TotalFee, &op.ReserveRecovered, &op.Status, &op.DryRun,
		pq.Array(&op.TransactionHashes), &op.ActorType, &op.ActorID, &op.ErrorMessage,
		&op.CreatedAt, &op.CompletedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found is valid for idempotency check
	}
	return op, err
}

func (r *consolidationRepository) UpdateStatus(ctx context.Context, id, status string, txHashes []string, errMsg error) error {
	now := time.Now()
	var errorMessage *string
	if errMsg != nil {
		msg := errMsg.Error()
		errorMessage = &msg
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE consolidation_operations
		SET status = $1, transaction_hashes = $2, error_message = $3, completed_at = $4
		WHERE id = $5
	`, status, pq.Array(txHashes), errorMessage, now, id)

	return err
}

func (r *consolidationRepository) List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.ConsolidationOperation, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, source_wallet_ids, destination_wallet, idempotency_key,
		       total_fee, reserve_recovered, status, dry_run, transaction_hashes,
		       actor_type, actor_id, error_message, created_at, completed_at
		FROM consolidation_operations
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, tenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var operations []*domain.ConsolidationOperation
	for rows.Next() {
		op := &domain.ConsolidationOperation{}
		err := rows.Scan(
			&op.ID, &op.TenantID, pq.Array(&op.SourceWalletIDs), &op.DestinationWallet,
			&op.IdempotencyKey, &op.TotalFee, &op.ReserveRecovered, &op.Status, &op.DryRun,
			pq.Array(&op.TransactionHashes), &op.ActorType, &op.ActorID, &op.ErrorMessage,
			&op.CreatedAt, &op.CompletedAt,
		)
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}

	return operations, rows.Err()
}
