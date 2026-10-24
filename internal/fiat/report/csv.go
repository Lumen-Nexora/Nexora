package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

var settlementAmount = regexp.MustCompile(`^[0-9]{1,20}(\.[0-9]{1,7})?$`)
var settlementCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

// ReadSettlementReport ingests a normalized provider CSV, not webhook events.
// Provider-specific exports must map to these columns before ingestion.
func ReadSettlementReport(input io.Reader, day time.Time) ([]domain.BankSettlement, error) {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read settlement header: %w", err)
	}
	expected := []string{"reference", "amount", "currency", "status", "settled_at"}
	if len(header) != len(expected) {
		return nil, fmt.Errorf("expected columns: %s", strings.Join(expected, ","))
	}
	for i := range header {
		if header[i] != expected[i] {
			return nil, fmt.Errorf("column %d must be %s", i+1, expected[i])
		}
	}
	start := day.UTC().Truncate(24 * time.Hour)
	result := make([]domain.BankSettlement, 0)
	for line := 2; ; line++ {
		row, err := reader.Read()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, fmt.Errorf("settlement row %d: %w", line, err)
		}
		if len(result) >= 10000 {
			return nil, fmt.Errorf("settlement report exceeds 10000 rows")
		}
		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}
		if row[0] == "" || len(row[0]) > 256 {
			return nil, fmt.Errorf("row %d: invalid reference", line)
		}
		if !settlementAmount.MatchString(row[1]) || !settlementCurrency.MatchString(row[2]) {
			return nil, fmt.Errorf("row %d: invalid amount or currency", line)
		}
		amount, err := decimal.NewFromString(row[1])
		if err != nil {
			return nil, fmt.Errorf("row %d amount: %w", line, err)
		}
		if !amount.IsPositive() {
			return nil, fmt.Errorf("row %d: amount must be positive", line)
		}
		if row[3] != "settled" && row[3] != "pending" && row[3] != "failed" {
			return nil, fmt.Errorf("row %d: unsupported status", line)
		}
		at, err := time.Parse(time.RFC3339, row[4])
		if err != nil {
			return nil, fmt.Errorf("row %d settlement time: %w", line, err)
		}
		if at.Before(start) || !at.Before(start.AddDate(0, 0, 1)) {
			return nil, fmt.Errorf("row %d: settlement time outside requested UTC day", line)
		}
		result = append(result, domain.BankSettlement{Reference: row[0], Amount: amount, Currency: row[2], Status: row[3], SettledAt: at.UTC()})
	}
}
