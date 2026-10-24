package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/jackc/pgx/v5"
)

// TravelRuleRepo stores only the encrypted envelope and signed confirmation
// metadata. Plaintext IVMS 101 data is never written to this table.
type TravelRuleRepo struct {
	db DB
}

func NewTravelRuleRepo(db DB) *TravelRuleRepo {
	return &TravelRuleRepo{db: db}
}

func (r *TravelRuleRepo) CreateTravelRuleExchange(ctx context.Context, exchange *domain.TravelRuleExchange) error {
	var tenantID interface{}
	if exchange.TenantID != "" {
		tenantID = exchange.TenantID
	}
	_, err := r.db.Exec(ctx,
		"INSERT INTO travel_rule_exchanges "+
			"(message_id, transaction_id, tenant_id, mode, recipient_vasp_id, protocol, envelope_sha256, encrypted_envelope, status, release_status, created_at, updated_at) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11, $12)",
		exchange.MessageID, exchange.TransferID, tenantID, exchange.Mode, exchange.RecipientVASPID,
		exchange.Protocol, exchange.EnvelopeSHA256, string(exchange.EnvelopeJSON), exchange.Status,
		exchange.ReleaseStatus, exchange.CreatedAt, exchange.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert Travel Rule exchange: %w", err)
	}
	return nil
}

func (r *TravelRuleRepo) GetTravelRuleExchange(ctx context.Context, transferID string) (*domain.TravelRuleExchange, error) {
	var exchange domain.TravelRuleExchange
	var tenantID *string
	var mode, status, releaseStatus string
	var confirmedAt *time.Time
	err := r.db.QueryRow(ctx,
		"SELECT message_id::text, transaction_id::text, tenant_id::text, mode, recipient_vasp_id, protocol, "+
			"encrypted_envelope, envelope_sha256, status, release_status, COALESCE(confirmation_id, ''), created_at, updated_at, confirmed_at "+
			"FROM travel_rule_exchanges WHERE transaction_id = $1",
		transferID,
	).Scan(&exchange.MessageID, &exchange.TransferID, &tenantID, &mode, &exchange.RecipientVASPID,
		&exchange.Protocol, &exchange.EnvelopeJSON, &exchange.EnvelopeSHA256, &status, &releaseStatus,
		&exchange.ConfirmationID, &exchange.CreatedAt, &exchange.UpdatedAt, &confirmedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("Travel Rule exchange not found: %w", pgx.ErrNoRows)
		}
		return nil, fmt.Errorf("get Travel Rule exchange: %w", err)
	}
	if tenantID != nil {
		exchange.TenantID = *tenantID
	}
	exchange.Mode = domain.Mode(mode)
	exchange.Status = domain.TravelRuleExchangeStatus(status)
	exchange.ReleaseStatus = domain.TransactionStatus(releaseStatus)
	exchange.ConfirmedAt = confirmedAt
	return &exchange, nil
}

func (r *TravelRuleRepo) SaveTravelRuleConfirmation(ctx context.Context, transferID string, status domain.TravelRuleExchangeStatus, confirmationID string, at time.Time) error {
	if status != domain.TravelRuleExchangeConfirmed && status != domain.TravelRuleExchangeRejected {
		return errors.New("invalid Travel Rule confirmation status")
	}
	tag, err := r.db.Exec(ctx,
		"UPDATE travel_rule_exchanges SET status = $2, confirmation_id = $3, confirmed_at = $4, updated_at = NOW() "+
			"WHERE transaction_id = $1 AND status = 'pending'",
		transferID, status, confirmationID, at,
	)
	if err != nil {
		return fmt.Errorf("save Travel Rule confirmation: %w", err)
	}
	if tag.RowsAffected() != 0 {
		return nil
	}
	var existingStatus, existingID string
	err = r.db.QueryRow(ctx,
		"SELECT status, COALESCE(confirmation_id, '') FROM travel_rule_exchanges WHERE transaction_id = $1",
		transferID,
	).Scan(&existingStatus, &existingID)
	if err != nil {
		return fmt.Errorf("read existing Travel Rule confirmation: %w", err)
	}
	if existingStatus == string(status) && existingID == confirmationID {
		return nil
	}
	return errors.New("conflicting Travel Rule confirmation for transfer")
}
