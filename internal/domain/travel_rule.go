package domain

import (
	"encoding/json"
	"time"
)

// TravelRuleRequest contains standardized IVMS 101 party data and the
// recipient VASP identifier from the operator-managed trust directory.
// Endpoints and public keys are never accepted from the transfer caller.
type TravelRuleRequest struct {
	RecipientVASPID string          `json:"recipient_vasp_id"`
	IVMS101         json.RawMessage `json:"ivms101"`
}

type TravelRuleExchangeStatus string

const (
	TravelRuleExchangePending   TravelRuleExchangeStatus = "pending"
	TravelRuleExchangeConfirmed TravelRuleExchangeStatus = "confirmed"
	TravelRuleExchangeRejected  TravelRuleExchangeStatus = "rejected"
)

// TravelRuleConfirmation is signed by the recipient VASP's trusted Ed25519
// confirmation key. The signature covers every field except Signature.
type TravelRuleConfirmation struct {
	ConfirmationID string    `json:"confirmation_id"`
	MessageID      string    `json:"message_id"`
	TransferID     string    `json:"transfer_id"`
	VASPID         string    `json:"vasp_id"`
	Status         string    `json:"status"`
	EnvelopeSHA256 string    `json:"envelope_sha256"`
	IssuedAt       time.Time `json:"issued_at"`
	Signature      string    `json:"signature"`
}

// TravelRuleExchange persists only the encrypted envelope, never plaintext
// originator or beneficiary PII.
type TravelRuleExchange struct {
	MessageID       string
	TransferID      string
	TenantID        string
	Mode            Mode
	RecipientVASPID string
	Protocol        string
	EnvelopeJSON    []byte
	EnvelopeSHA256  string
	Status          TravelRuleExchangeStatus
	ReleaseStatus   TransactionStatus
	ConfirmationID  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ConfirmedAt     *time.Time
}
