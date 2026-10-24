package fiat

import (
	"io"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fiat/report"
)

// ReadSettlementReport is independent of payment initiation and webhook handling.
func ReadSettlementReport(input io.Reader, day time.Time) ([]domain.BankSettlement, error) {
	return report.ReadSettlementReport(input, day)
}
