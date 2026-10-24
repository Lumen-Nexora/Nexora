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

// Consolidation lifecycle states.
const (
	ConsolidationPending            = "pending"
	ConsolidationProcessing         = "processing"
	ConsolidationCompleted          = "completed"
	ConsolidationPartiallyCompleted = "partially_completed"
	ConsolidationFailed             = "failed"
)

// ConsolidationOperation aggregates balances from many of a tenant's wallets
// into one of its wallets, in bounded batches of 10-20 wallets per Stellar
// transaction.
type ConsolidationOperation struct {
	ID                string               `json:"id"`
	TenantID          string               `json:"tenant_id"`
	Mode              Mode                 `json:"mode"`
	SourceWalletIDs   []string             `json:"source_wallet_ids"`
	DestinationWallet string               `json:"destination_wallet"`
	IdempotencyKey    string               `json:"idempotency_key"`
	MergeSources      bool                 `json:"merge_sources"`
	BatchSize         int                  `json:"batch_size"`
	TotalFee          decimal.Decimal      `json:"total_fee"`
	ReserveRecovered  decimal.Decimal      `json:"reserve_recovered"`
	Status            string               `json:"status"`
	DryRun            bool                 `json:"dry_run"`
	TransactionHashes []string             `json:"transaction_hashes,omitempty"`
	Batches           []ConsolidationBatch `json:"batches"`
	ActorType         string               `json:"actor_type"`
	ActorID           string               `json:"actor_id"`
	ErrorMessage      *string              `json:"error_message,omitempty"`
	// Fingerprint identifies the request body behind IdempotencyKey.
	Fingerprint string     `json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// ConsolidationBatch is the outcome of one consolidation transaction.
type ConsolidationBatch struct {
	Index           int      `json:"index"`
	WalletIDs       []string `json:"wallet_ids"`
	Operations      int      `json:"operations"`
	FeeStroops      int64    `json:"fee_stroops"`
	FeePayer        string   `json:"fee_payer_wallet_id"`
	TransactionHash string   `json:"transaction_hash,omitempty"`
	Status          string   `json:"status,omitempty"` // completed or failed; empty in a preview
	Error           string   `json:"error,omitempty"`
}

// ConsolidationPreview is the dry-run result: exactly what a consolidation
// would submit right now, without submitting it.
type ConsolidationPreview struct {
	DestinationWallet string                    `json:"destination_wallet"`
	MergeSources      bool                      `json:"merge_sources"`
	BatchSize         int                       `json:"batch_size"`
	Batches           []ConsolidationBatch      `json:"batches"`
	Wallets           []ConsolidationWalletPlan `json:"wallets"`
	TotalFee          string                    `json:"total_fee"`
	TotalFeeStroops   int64                     `json:"total_fee_stroops"`
	ReserveRecovered  string                    `json:"reserve_recovered"`
	// NetXLM is ReserveRecovered minus TotalFee; negative means the
	// consolidation costs more in fees than it frees.
	NetXLM            string                     `json:"net_xlm"`
	ResultingBalances []ConsolidationAssetChange `json:"resulting_balances"`
}

// ConsolidationWalletPlan is what happens to one source wallet.
type ConsolidationWalletPlan struct {
	WalletID  string      `json:"wallet_id"`
	PublicKey string      `json:"public_key"`
	Moves     []SweepItem `json:"moves,omitempty"`
	Merged    bool        `json:"merged"`
	// SkipReason is set when the wallet is left out; nothing moves from it.
	SkipReason string `json:"skip_reason,omitempty"`
}

// ConsolidationAssetChange is the destination wallet's balance of one asset
// before and after the consolidation.
type ConsolidationAssetChange struct {
	AssetCode string `json:"asset_code"`
	Issuer    string `json:"issuer,omitempty"`
	Before    string `json:"before"`
	After     string `json:"after"`
}

// Account closure lifecycle states.
const (
	ClosureStatusPending   = "pending"
	ClosureStatusCompleted = "completed"
	ClosureStatusFailed    = "failed"
)

// AccountClosure is the permanent record of a destructive custodial account
// closure: the trustlines removed, the AccountMerge that swept the remaining
// native XLM to the tenant master address, and who authorised it and why.
type AccountClosure struct {
	ID                 string            `json:"id"`
	TenantID           string            `json:"tenant_id"`
	Mode               Mode              `json:"mode"`
	WalletID           string            `json:"wallet_id"`
	DestinationWallet  *string           `json:"destination_wallet,omitempty"`
	DestinationAddress string            `json:"destination_address"`
	ActorType          string            `json:"actor_type"`
	ActorID            string            `json:"actor_id"`
	Justification      string            `json:"justification"`
	RemovedTrustlines  []ClosedTrustline `json:"removed_trustlines"`
	NativeBalance      *decimal.Decimal  `json:"native_balance,omitempty"`
	TransactionHash    *string           `json:"transaction_hash,omitempty"`
	Status             string            `json:"status"` // pending, completed, failed
	ErrorMessage       *string           `json:"error_message,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	CompletedAt        *time.Time        `json:"completed_at,omitempty"`
}

// ClosedTrustline identifies one asset trustline removed during a closure.
type ClosedTrustline struct {
	AssetCode string `json:"asset_code"`
	Issuer    string `json:"issuer"`
}

// TenantCustodySettings holds the per-tenant, per-mode accounts that the
// closure protocol depends on.
type TenantCustodySettings struct {
	TenantID string `json:"tenant_id"`
	Mode     Mode   `json:"mode"`
	// MasterAddress receives the native XLM reserves of every closed wallet.
	MasterAddress string `json:"master_address"`
	// PrimarySettlementWalletID is the wallet the tenant settles through; it
	// can never be closed.
	PrimarySettlementWalletID *string `json:"primary_settlement_wallet_id,omitempty"`
	// AutoSweepEnabled opts the tenant's custodial wallets in to the
	// automated reserve sweep into MasterAddress.
	AutoSweepEnabled bool      `json:"auto_sweep_enabled"`
	UpdatedBy        string    `json:"updated_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Custodial sweep transaction states.
const (
	SweepStatusCompleted = "completed"
	SweepStatusFailed    = "failed"
)

// CustodialSweep records one batched sweep transaction that moved customer
// deposits from custodial wallets into the tenant master address.
type CustodialSweep struct {
	ID                 string      `json:"id"`
	TenantID           string      `json:"tenant_id"`
	Mode               Mode        `json:"mode"`
	DestinationAddress string      `json:"destination_address"`
	TransactionHash    *string     `json:"transaction_hash,omitempty"`
	Status             string      `json:"status"`
	FeeStroops         int64       `json:"fee_stroops"`
	Items              []SweepItem `json:"items"`
	ErrorMessage       *string     `json:"error_message,omitempty"`
	CreatedAt          time.Time   `json:"created_at"`
}

// SweepItem is one asset moved out of one wallet by a sweep.
type SweepItem struct {
	WalletID  string `json:"wallet_id"`
	PublicKey string `json:"public_key"`
	AssetCode string `json:"asset_code"`
	Issuer    string `json:"issuer,omitempty"`
	// Amount is what was sent to the master address.
	Amount string `json:"amount"`
	// BalanceBefore is the wallet's balance of the asset when it was scanned.
	BalanceBefore string `json:"balance_before"`
	// Retained is the wallet's balance of the asset after the sweep: the
	// minimum reserve and selling liabilities for XLM, open selling
	// liabilities for other assets. The network fee is reported separately.
	Retained string `json:"retained"`
}
