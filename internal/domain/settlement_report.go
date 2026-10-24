package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// BankSettlement is a normalized row from a provider's daily report.
type BankSettlement struct {
	Reference string
	Amount    decimal.Decimal
	Currency  string
	Status    string
	SettledAt time.Time
}
