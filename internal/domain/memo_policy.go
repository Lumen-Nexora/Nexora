package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// MemoFormat identifies the shape a tenant expects an inbound memo to have.
// A deposit whose memo does not satisfy the format is routed to the pending
// investigation queue instead of being credited to a customer.
type MemoFormat string

const (
	// MemoFormatCustomerID expects an opaque customer reference such as
	// "cust_9f2b". Stellar caps text memos at 28 bytes, so a customer
	// reference has to fit in that budget.
	MemoFormatCustomerID MemoFormat = "customer_id"
	// MemoFormatEmail expects an email address.
	MemoFormatEmail MemoFormat = "email"
	// MemoFormatPhone expects an E.164 phone number.
	MemoFormatPhone MemoFormat = "phone"
	// MemoFormatReference expects an alphanumeric reference with optional
	// separators.
	MemoFormatReference MemoFormat = "reference"
	// MemoFormatCustom expects the memo to match the policy's own pattern.
	MemoFormatCustom MemoFormat = "custom"
)

// ValidMemoFormat reports whether f is a format the validator understands.
func ValidMemoFormat(f MemoFormat) bool {
	switch f {
	case MemoFormatCustomerID, MemoFormatEmail, MemoFormatPhone, MemoFormatReference, MemoFormatCustom:
		return true
	default:
		return false
	}
}

// MemoPolicy is the inbound memo rule a tenant applies to one custodial
// deposit account. It is scoped to a single wallet: shared deposit accounts
// are the ones where an unmapped memo is most likely to strand funds.
type MemoPolicy struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"`
	WalletID    string     `json:"wallet_id"`
	Format      MemoFormat `json:"format"`
	Pattern     string     `json:"pattern,omitempty"`
	RequireMemo bool       `json:"require_memo"`
	AutoRefund  bool       `json:"auto_refund"`
	// RefundAfter is the timeout before an unmapped deposit is returned to
	// its sender. It is not serialized: the API expresses it in seconds.
	RefundAfter time.Duration `json:"-"`
	Enabled     bool          `json:"enabled"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// UnmappedDepositReason explains why a deposit could not be mapped to a
// customer.
type UnmappedDepositReason string

const (
	// UnmappedDepositReasonMissing means the payment carried no memo.
	UnmappedDepositReasonMissing UnmappedDepositReason = "missing_memo"
	// UnmappedDepositReasonInvalid means the memo did not satisfy the
	// wallet's memo policy.
	UnmappedDepositReasonInvalid UnmappedDepositReason = "invalid_memo"
)

// UnmappedDepositStatus is the resolution state of a queued deposit.
type UnmappedDepositStatus string

const (
	// UnmappedDepositPending means the deposit is awaiting investigation.
	UnmappedDepositPending UnmappedDepositStatus = "pending"
	// UnmappedDepositRefunding means a worker claimed the deposit and is
	// returning it to its sender.
	UnmappedDepositRefunding UnmappedDepositStatus = "refunding"
	// UnmappedDepositRefunded means the deposit was returned to the sender.
	UnmappedDepositRefunded UnmappedDepositStatus = "refunded"
	// UnmappedDepositResolved means an operator closed the case.
	UnmappedDepositResolved UnmappedDepositStatus = "resolved"
)

// UnmappedDeposit is an inbound payment that arrived on a wallet with a memo
// policy but whose memo could not be mapped to a customer. The row is both
// the pending investigation queue entry and the durable audit trail of the
// deposit.
type UnmappedDeposit struct {
	ID            string                `json:"id"`
	TenantID      string                `json:"tenant_id"`
	WalletID      string                `json:"wallet_id"`
	Mode          Mode                  `json:"mode"`
	TxHash        string                `json:"tx_hash"`
	SourceAddress string                `json:"source_address"`
	Asset         string                `json:"asset"`
	Issuer        string                `json:"issuer,omitempty"`
	Amount        decimal.Decimal       `json:"amount"`
	Memo          string                `json:"memo"`
	Reason        UnmappedDepositReason `json:"reason"`
	Status        UnmappedDepositStatus `json:"status"`
	RefundAfter   time.Time             `json:"refund_after"`
	RefundTxHash  string                `json:"refund_tx_hash,omitempty"`
	Resolution    string                `json:"resolution,omitempty"`
	FailureReason string                `json:"failure_reason,omitempty"`
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
	ResolvedAt    *time.Time            `json:"resolved_at,omitempty"`
}

// MemoEvaluation is the outcome of matching an inbound memo against the
// destination wallet's memo policy. A nil evaluation means the wallet has no
// policy, so the deposit is mapped by default.
type MemoEvaluation struct {
	PolicyID   string                `json:"policy_id"`
	Format     MemoFormat            `json:"format"`
	Matched    bool                  `json:"matched"`
	Reason     UnmappedDepositReason `json:"reason,omitempty"`
	CustomerID string                `json:"customer_id,omitempty"`
}
