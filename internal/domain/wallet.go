package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// CustodyType distinguishes custodial wallets (Nexora holds the encrypted
// secret) from contract wallets (a Soroban contract holds the funds and
// enforces spending policy on-chain).
type CustodyType string

const (
	CustodyCustodial CustodyType = "custodial"
	CustodyContract  CustodyType = "contract"
)

type Wallet struct {
	ID              string      `json:"id"`
	TenantID        *string     `json:"tenant_id,omitempty"`
	PublicKey       string      `json:"public_key"`
	Mode            Mode        `json:"mode"`
	EncryptedSecret string      `json:"-"`
	SyncCursor      string      `json:"-"`
	CustodyType     CustodyType `json:"custody_type"`
	ContractID      string      `json:"contract_id,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
}

// BalanceRecord is one cached on-chain balance for a wallet/asset pair.
type BalanceRecord struct {
	WalletID  string    `json:"wallet_id"`
	AssetCode string    `json:"asset_code"`
	Issuer    string    `json:"issuer"`
	Balance   string    `json:"balance"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Balance struct {
	AssetCode   string `json:"asset_code"`
	AssetIssuer string `json:"asset_issuer,omitempty"`
	Balance     string `json:"balance"`
	Limit       string `json:"limit,omitempty"`
}

type WalletBalance struct {
	WalletID  string    `json:"wallet_id"`
	Balances  []Balance `json:"balances"`
	Stale     bool      `json:"stale"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConsolidationOperation represents a request to consolidate balances from multiple wallets.
type ConsolidationOperation struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	SourceWalletIDs   []string        `json:"source_wallet_ids"`
	DestinationWallet string          `json:"destination_wallet"`
	IdempotencyKey    string          `json:"idempotency_key"`
	TotalFee          decimal.Decimal `json:"total_fee"`
	ReserveRecovered  decimal.Decimal `json:"reserve_recovered"`
	Status            string          `json:"status"` // pending, completed, failed
	DryRun            bool            `json:"dry_run"`
	TransactionHashes []string        `json:"transaction_hashes,omitempty"`
	ActorType         string          `json:"actor_type"`
	ActorID           string          `json:"actor_id"`
	ErrorMessage      *string         `json:"error_message,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	CompletedAt       *time.Time      `json:"completed_at,omitempty"`
}

// AccountClosure represents a request to close a custodial account.
type AccountClosure struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id"`
	WalletID          string     `json:"wallet_id"`
	DestinationWallet string     `json:"destination_wallet"`
	ActorType         string     `json:"actor_type"`
	ActorID           string     `json:"actor_id"`
	TransactionHash   *string    `json:"transaction_hash,omitempty"`
	Status            string     `json:"status"` // pending, completed, failed
	ErrorMessage      *string    `json:"error_message,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
}
