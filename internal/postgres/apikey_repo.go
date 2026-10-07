package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/jackc/pgx/v5"
)

type APIKeyRepo struct {
	db DB
}

func NewAPIKeyRepo(db DB) *APIKeyRepo {
	return &APIKeyRepo{db: db}
}

func (r *APIKeyRepo) Create(ctx context.Context, key *domain.APIKey) error {
	if !key.Mode.Valid() {
		return errors.New("api key mode must be live or test")
	}
	if key.Scopes == nil {
		key.Scopes = []string{}
	}
	if key.RotationReminderDays <= 0 {
		key.RotationReminderDays = 7
	}
	db := TxFromContext(ctx, r.db)
	_, err := db.Exec(ctx,
		`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, mode, label, role, scopes, expires_at, rotation_reminder_days, last_rotation_reminded_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		key.ID, key.TenantID, key.KeyHash, key.Prefix, key.Mode, key.Label, key.Role, key.Scopes, key.ExpiresAt, key.RotationReminderDays, key.LastRotationRemindedAt, key.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (r *APIKeyRepo) GetByHash(ctx context.Context, hash string) (*domain.APIKey, error) {
	k := &domain.APIKey{}
	err := r.db.QueryRow(ctx,
		`SELECT id, tenant_id, key_hash, prefix, mode, label, role, COALESCE(scopes, '{}'), last_used_at, revoked_at, expires_at, COALESCE(rotation_reminder_days, 7), last_rotation_reminded_at, created_at FROM api_keys WHERE key_hash = $1`,
		hash,
	).Scan(&k.ID, &k.TenantID, &k.KeyHash, &k.Prefix, &k.Mode, &k.Label, &k.Role, &k.Scopes, &k.LastUsedAt, &k.RevokedAt, &k.ExpiresAt, &k.RotationReminderDays, &k.LastRotationRemindedAt, &k.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("api key not found")
		}
		return nil, fmt.Errorf("get api_key by hash: %w", err)
	}
	return k, nil
}

func (r *APIKeyRepo) GetByID(ctx context.Context, id, tenantID string, mode domain.Mode) (*domain.APIKey, error) {
	k := &domain.APIKey{}
	err := r.db.QueryRow(ctx,
		`SELECT id, tenant_id, key_hash, prefix, mode, label, role, COALESCE(scopes, '{}'), last_used_at, revoked_at, expires_at, COALESCE(rotation_reminder_days, 7), last_rotation_reminded_at, created_at FROM api_keys WHERE id = $1 AND tenant_id = $2 AND mode = $3`,
		id, tenantID, mode,
	).Scan(&k.ID, &k.TenantID, &k.KeyHash, &k.Prefix, &k.Mode, &k.Label, &k.Role, &k.Scopes, &k.LastUsedAt, &k.RevokedAt, &k.ExpiresAt, &k.RotationReminderDays, &k.LastRotationRemindedAt, &k.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("api key not found")
		}
		return nil, fmt.Errorf("get api_key by id: %w", err)
	}
	return k, nil
}

// ListByTenant returns the keys for one environment. Keys are environment
// scoped: a live key must never be listed as if it could authenticate against
// testnet.
func (r *APIKeyRepo) ListByTenant(ctx context.Context, tenantID string, mode domain.Mode) ([]*domain.APIKey, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, tenant_id, key_hash, prefix, mode, label, role, COALESCE(scopes, '{}'), last_used_at, revoked_at, expires_at, COALESCE(rotation_reminder_days, 7), last_rotation_reminded_at, created_at
		 FROM api_keys WHERE tenant_id = $1 AND mode = $2 ORDER BY created_at DESC`,
		tenantID, mode,
	)
	if err != nil {
		return nil, fmt.Errorf("list api_keys: %w", err)
	}
	defer rows.Close()

	var keys []*domain.APIKey
	for rows.Next() {
		k := &domain.APIKey{}
		if err := rows.Scan(&k.ID, &k.TenantID, &k.KeyHash, &k.Prefix, &k.Mode, &k.Label, &k.Role, &k.Scopes, &k.LastUsedAt, &k.RevokedAt, &k.ExpiresAt, &k.RotationReminderDays, &k.LastRotationRemindedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (r *APIKeyRepo) Revoke(ctx context.Context, id string, tenantID string, mode domain.Mode) error {
	res, err := r.db.Exec(ctx,
		`UPDATE api_keys SET revoked_at = NOW() WHERE id = $1 AND tenant_id = $2 AND mode = $3`,
		id, tenantID, mode,
	)
	if err != nil {
		return fmt.Errorf("revoke api_key: %w", err)
	}
	if res.RowsAffected() == 0 {
		return errors.New("api key not found or not owned by tenant")
	}
	return nil
}

func (r *APIKeyRepo) UpdateLastUsed(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`,
		id,
	)
	if err != nil {
		return fmt.Errorf("update api_key last_used: %w", err)
	}
	return nil
}

// UpdateExpiry updates or removes the expiration timestamp and rotation reminder window.
func (r *APIKeyRepo) UpdateExpiry(ctx context.Context, id, tenantID string, mode domain.Mode, expiresAt *time.Time, reminderDays int) error {
	if reminderDays <= 0 {
		reminderDays = 7
	}
	res, err := r.db.Exec(ctx,
		`UPDATE api_keys
		 SET expires_at = $1, rotation_reminder_days = $2
		 WHERE id = $3 AND tenant_id = $4 AND mode = $5 AND revoked_at IS NULL`,
		expiresAt, reminderDays, id, tenantID, mode,
	)
	if err != nil {
		return fmt.Errorf("update api_key expiry: %w", err)
	}
	if res.RowsAffected() == 0 {
		return errors.New("api key not found, already revoked, or not owned by tenant")
	}
	return nil
}

// RecordRotationReminder records that a rotation reminder was dispatched at the given time.
func (r *APIKeyRepo) RecordRotationReminder(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE api_keys SET last_rotation_reminded_at = $1 WHERE id = $2`,
		at, id,
	)
	if err != nil {
		return fmt.Errorf("record rotation reminder: %w", err)
	}
	return nil
}

// ListExpiringKeys returns all active, unrevoked keys across all tenants that have an expiry date.
func (r *APIKeyRepo) ListExpiringKeys(ctx context.Context, limit int) ([]*domain.APIKey, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.Query(ctx,
		`SELECT id, tenant_id, key_hash, prefix, mode, label, role, COALESCE(scopes, '{}'), last_used_at, revoked_at, expires_at, COALESCE(rotation_reminder_days, 7), last_rotation_reminded_at, created_at
		 FROM api_keys
		 WHERE revoked_at IS NULL AND expires_at IS NOT NULL
		 ORDER BY expires_at ASC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expiring api_keys: %w", err)
	}
	defer rows.Close()

	var keys []*domain.APIKey
	for rows.Next() {
		k := &domain.APIKey{}
		if err := rows.Scan(&k.ID, &k.TenantID, &k.KeyHash, &k.Prefix, &k.Mode, &k.Label, &k.Role, &k.Scopes, &k.LastUsedAt, &k.RevokedAt, &k.ExpiresAt, &k.RotationReminderDays, &k.LastRotationRemindedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
