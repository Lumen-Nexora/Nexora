package fx

import (
	"context"
	"database/sql"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
)

// LockRepository handles persistence of FX rate locks.
type LockRepository interface {
	Create(ctx context.Context, lock *domain.RateLock) error
	Get(ctx context.Context, id, tenantID string) (*domain.RateLock, error)
	ConsumeLock(ctx context.Context, lockID, conversionID string) error
	CleanupExpired(ctx context.Context) (int64, error)
}

type lockRepository struct {
	db *sql.DB
}

// NewLockRepository creates a new rate lock repository.
func NewLockRepository(db *sql.DB) LockRepository {
	return &lockRepository{db: db}
}

func (r *lockRepository) Create(ctx context.Context, lock *domain.RateLock) error {
	lock.ID = uuid.New().String()
	lock.CreatedAt = time.Now()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fx_rate_locks 
		(id, tenant_id, from_asset, to_asset, locked_rate, amount, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, lock.ID, lock.TenantID, lock.FromAsset, lock.ToAsset, lock.LockedRate,
		lock.Amount, lock.ExpiresAt, lock.CreatedAt)

	return err
}

func (r *lockRepository) Get(ctx context.Context, id, tenantID string) (*domain.RateLock, error) {
	lock := &domain.RateLock{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, from_asset, to_asset, locked_rate, amount,
		       expires_at, consumed_at, consumed_by, created_at
		FROM fx_rate_locks
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID).Scan(
		&lock.ID, &lock.TenantID, &lock.FromAsset, &lock.ToAsset, &lock.LockedRate,
		&lock.Amount, &lock.ExpiresAt, &lock.ConsumedAt, &lock.ConsumedBy, &lock.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, domain.ErrWalletNotFound
	}
	return lock, err
}

func (r *lockRepository) ConsumeLock(ctx context.Context, lockID, conversionID string) error {
	now := time.Now()
	result, err := r.db.ExecContext(ctx, `
		UPDATE fx_rate_locks
		SET consumed_at = $1, consumed_by = $2
		WHERE id = $3 AND consumed_at IS NULL AND expires_at > NOW()
	`, now, conversionID, lockID)

	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rows == 0 {
		return domain.ErrQuoteExpired
	}

	return nil
}

func (r *lockRepository) CleanupExpired(ctx context.Context) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM fx_rate_locks
		WHERE consumed_at IS NULL AND expires_at < NOW()
	`)

	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}
