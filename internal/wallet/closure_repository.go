package wallet

import (
	"context"
	"database/sql"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
)

// ClosureRepository handles persistence of account closure operations.
type ClosureRepository interface {
	Create(ctx context.Context, closure *domain.AccountClosure) error
	Get(ctx context.Context, id, tenantID string) (*domain.AccountClosure, error)
	UpdateStatus(ctx context.Context, id, status, txHash string, err error) error
	List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.AccountClosure, error)
}

type closureRepository struct {
	db *sql.DB
}

// NewClosureRepository creates a new account closure repository.
func NewClosureRepository(db *sql.DB) ClosureRepository {
	return &closureRepository{db: db}
}

func (r *closureRepository) Create(ctx context.Context, closure *domain.AccountClosure) error {
	closure.ID = uuid.New().String()
	closure.CreatedAt = time.Now()
	closure.Status = "pending"

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO account_closures 
		(id, tenant_id, wallet_id, destination_wallet, actor_type, actor_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, closure.ID, closure.TenantID, closure.WalletID, closure.DestinationWallet,
		closure.ActorType, closure.ActorID, closure.Status, closure.CreatedAt)

	return err
}

func (r *closureRepository) Get(ctx context.Context, id, tenantID string) (*domain.AccountClosure, error) {
	closure := &domain.AccountClosure{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, wallet_id, destination_wallet, actor_type, actor_id,
		       transaction_hash, status, error_message, created_at, completed_at
		FROM account_closures
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID).Scan(
		&closure.ID, &closure.TenantID, &closure.WalletID, &closure.DestinationWallet,
		&closure.ActorType, &closure.ActorID, &closure.TransactionHash, &closure.Status,
		&closure.ErrorMessage, &closure.CreatedAt, &closure.CompletedAt,
	)

	if err == sql.ErrNoRows {
		return nil, domain.ErrWalletNotFound
	}
	return closure, err
}

func (r *closureRepository) UpdateStatus(ctx context.Context, id, status, txHash string, errMsg error) error {
	now := time.Now()
	var errorMessage *string
	if errMsg != nil {
		msg := errMsg.Error()
		errorMessage = &msg
	}

	var hash *string
	if txHash != "" {
		hash = &txHash
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE account_closures
		SET status = $1, transaction_hash = $2, error_message = $3, completed_at = $4
		WHERE id = $5
	`, status, hash, errorMessage, now, id)

	return err
}

func (r *closureRepository) List(ctx context.Context, tenantID string, limit, offset int) ([]*domain.AccountClosure, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, wallet_id, destination_wallet, actor_type, actor_id,
		       transaction_hash, status, error_message, created_at, completed_at
		FROM account_closures
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, tenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var closures []*domain.AccountClosure
	for rows.Next() {
		closure := &domain.AccountClosure{}
		err := rows.Scan(
			&closure.ID, &closure.TenantID, &closure.WalletID, &closure.DestinationWallet,
			&closure.ActorType, &closure.ActorID, &closure.TransactionHash, &closure.Status,
			&closure.ErrorMessage, &closure.CreatedAt, &closure.CompletedAt,
		)
		if err != nil {
			return nil, err
		}
		closures = append(closures, closure)
	}

	return closures, rows.Err()
}
