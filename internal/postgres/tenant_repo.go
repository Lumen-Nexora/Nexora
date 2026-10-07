package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/jackc/pgx/v5"
)

type TenantRepo struct {
	db DB
}

func NewTenantRepo(db DB) *TenantRepo {
	return &TenantRepo{db: db}
}

func (r *TenantRepo) Create(ctx context.Context, t *domain.Tenant) error {
	if t.AccountType == "" {
		t.AccountType = domain.AccountTypeIndividual
	}
	db := TxFromContext(ctx, r.db)
	_, err := db.Exec(ctx,
		`INSERT INTO tenants (id, name, email, account_type, max_wallets, max_transfers_per_month, max_transfers_per_day, max_withdrawals_per_day, max_webhooks, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t.ID, t.Name, t.Email, t.AccountType, t.MaxWallets, t.MaxTransfersPerMonth, t.MaxTransfersPerDay, t.MaxWithdrawalsPerDay, t.MaxWebhooks, t.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert tenant: %w", err)
	}
	return nil
}

func (r *TenantRepo) GetByID(ctx context.Context, id string) (*domain.Tenant, error) {
	t := &domain.Tenant{}
	err := r.db.QueryRow(ctx,
		`SELECT id, name, email, account_type, max_wallets, max_transfers_per_month, max_transfers_per_day, max_withdrawals_per_day, max_webhooks, created_at
		 FROM tenants WHERE id = $1`,
		id,
	).Scan(&t.ID, &t.Name, &t.Email, &t.AccountType, &t.MaxWallets, &t.MaxTransfersPerMonth, &t.MaxTransfersPerDay, &t.MaxWithdrawalsPerDay, &t.MaxWebhooks, &t.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("tenant not found")
		}
		return nil, fmt.Errorf("get tenant by id: %w", err)
	}
	return t, nil
}

func (r *TenantRepo) Update(ctx context.Context, t *domain.Tenant) error {
	db := TxFromContext(ctx, r.db)
	_, err := db.Exec(ctx,
		`UPDATE tenants SET name = $2, email = $3, account_type = $4, max_wallets = $5, max_transfers_per_month = $6, max_transfers_per_day = $7, max_withdrawals_per_day = $8, max_webhooks = $9 WHERE id = $1`,
		t.ID, t.Name, t.Email, t.AccountType, t.MaxWallets, t.MaxTransfersPerMonth, t.MaxTransfersPerDay, t.MaxWithdrawalsPerDay, t.MaxWebhooks,
	)
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	return nil
}
