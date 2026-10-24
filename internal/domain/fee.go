package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

type FeeSchedule struct {
	ID               string
	TenantID         *string
	Asset            string
	TransferFeeBps   int
	ConversionFeeBps int
	MinFeeAmount     decimal.Decimal
	MaxFeeAmount     *decimal.Decimal
	CreatedAt        time.Time
}

type FeeTier struct {
	ID               string
	TenantID         *string
	MinVolume        decimal.Decimal
	TransferFeeBps   int
	ConversionFeeBps int
}

type FeeCollection struct {
	ID            string
	TransactionID string
	TenantID      *string
	Asset         string
	FeeAmount     decimal.Decimal
	FeeBps        int
	CollectedAt   time.Time
	CreatedAt     time.Time
}

type FeeCollectionSummary struct {
	Asset      string
	TotalFees  decimal.Decimal
	TenantFees []TenantFeeTotal
}

type TenantFeeTotal struct {
	TenantID  *string
	TotalFees decimal.Decimal
}

// NetworkFeeAllocation identifies who bears an on-chain network fee.
type NetworkFeeAllocation string

const (
	// NetworkFeeAllocationTenant attributes the cost to a tenant sub-account.
	NetworkFeeAllocationTenant NetworkFeeAllocation = "tenant"
	// NetworkFeeAllocationOverhead attributes the cost to platform overhead.
	NetworkFeeAllocationOverhead NetworkFeeAllocation = "platform_overhead"
)

// NetworkFeeCharge is one exact on-chain fee paid in stroops, valued at the
// historical XLM/USD rate observed at ledger close. XLMUSDRate and USDAmount are
// nil when no historical quote was available; Valued explicitly distinguishes
// that state from a genuine zero-cost charge.
type NetworkFeeCharge struct {
	ID                string
	TransactionID     *string
	TxHash            string
	TenantID          *string
	SubAccount        string
	CostCenter        string
	Allocation        NetworkFeeAllocation
	LedgerSequence    int32
	LedgerClosedAt    time.Time
	FeeChargedStroops int64
	XLMAmount         decimal.Decimal
	XLMUSDRate        *decimal.Decimal
	USDAmount         *decimal.Decimal
	Valued            bool
	CreatedAt         time.Time
}

// NetworkFeeUsage is a tenant's network-fee consumption over a period.
type NetworkFeeUsage struct {
	TenantID          string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	TransactionCount  int64
	FeeChargedStroops int64
	XLMAmount         decimal.Decimal
	USDAmount         decimal.Decimal
	UnvaluedCount     int64
}

// NetworkFeeCOGSLine is one tenant/sub-account/cost-center row of a monthly
// cost-of-goods-sold report.
type NetworkFeeCOGSLine struct {
	TenantID          *string
	SubAccount        string
	CostCenter        string
	Allocation        NetworkFeeAllocation
	TransactionCount  int64
	FeeChargedStroops int64
	XLMAmount         decimal.Decimal
	USDAmount         decimal.Decimal
	UnvaluedCount     int64
}

// NetworkFeeAllocationTotal aggregates a monthly COGS report by allocation.
type NetworkFeeAllocationTotal struct {
	Allocation        NetworkFeeAllocation
	TransactionCount  int64
	FeeChargedStroops int64
	XLMAmount         decimal.Decimal
	USDAmount         decimal.Decimal
	UnvaluedCount     int64
}

// NetworkFeeCOGSReport is the monthly blockchain network expenditure report.
type NetworkFeeCOGSReport struct {
	Month             string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	TransactionCount  int64
	FeeChargedStroops int64
	XLMAmount         decimal.Decimal
	USDAmount         decimal.Decimal
	UnvaluedCount     int64
	Lines             []NetworkFeeCOGSLine
	Allocations       []NetworkFeeAllocationTotal
}
