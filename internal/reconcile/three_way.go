package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
)

type ThreeWayRepository interface {
	ListFiatReconciliationTransfers(context.Context, string, time.Time, []string) ([]*domain.Transaction, error)
}

type ThreeWayService struct {
	repo    ThreeWayRepository
	clients stellar.ClientResolver
}

func NewThreeWayService(repo ThreeWayRepository, clients stellar.ClientResolver) *ThreeWayService {
	return &ThreeWayService{repo: repo, clients: clients}
}

type ThreeWayRow struct {
	Reference, TransferID, Hash, BankAmount, BankCurrency, InternalAmount, InternalCurrency, BankStatus, InternalStatus, ChainStatus string
	Findings                                                                                                                         []string
	Recommendation                                                                                                                   string
}

type ThreeWayReport struct {
	TenantID, Provider string
	Mode               domain.Mode
	Day, CheckedAt     time.Time
	Rows               []ThreeWayRow
}

// Run audits the day's internal transfer cohort plus older transfers referenced
// by the daily bank report. It never changes transfer state or moves money.
func (s *ThreeWayService) Run(ctx context.Context, provider string, day time.Time, bank []domain.BankSettlement) (*ThreeWayReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start reconciliation: %w", err)
	}
	tenantID := tenant.IDFromContext(ctx)
	mode, ok := tenant.ModeFromContext(ctx)
	if tenantID == "" || !ok {
		return nil, fmt.Errorf("tenant and mode are required")
	}
	if provider == "" || len(provider) > 64 || day.IsZero() {
		return nil, fmt.Errorf("provider and day are required")
	}
	if len(bank) > 10000 {
		return nil, fmt.Errorf("report exceeds 10000 rows")
	}
	day = day.UTC().Truncate(24 * time.Hour)
	refs := make([]string, 0, len(bank))
	byRef := make(map[string][]domain.BankSettlement)
	for _, row := range bank {
		byRef[row.Reference] = append(byRef[row.Reference], row)
		refs = append(refs, row.Reference)
	}
	txs, err := s.repo.ListFiatReconciliationTransfers(ctx, provider, day, refs)
	if err != nil {
		return nil, fmt.Errorf("read reconciliation transfers: %w", err)
	}
	if len(txs) > 10000 {
		return nil, fmt.Errorf("reconciliation exceeds 10000 transfers")
	}
	report := &ThreeWayReport{TenantID: tenantID, Mode: mode, Provider: provider, Day: day, CheckedAt: time.Now().UTC()}
	counts := make(map[string]int)
	hashes := make(map[string]int)
	for _, tx := range txs {
		if tx == nil || tx.TenantID == nil || *tx.TenantID != tenantID || tx.Mode != mode || tx.FiatRail == nil || *tx.FiatRail != provider || tx.FiatProviderRef == nil {
			return nil, fmt.Errorf("repository returned an unscoped fiat transfer")
		}
		counts[*tx.FiatProviderRef]++
		if tx.TxHash != "" {
			hashes[tx.TxHash]++
		}
	}
	client := s.clients.ClientForMode(ctx)
	chainCache := make(map[string]string)
	for _, tx := range txs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reconcile transfers: %w", err)
		}
		ref := *tx.FiatProviderRef
		row := ThreeWayRow{Reference: ref, TransferID: tx.ID, Hash: tx.TxHash, InternalStatus: string(tx.Status), ChainStatus: "missing"}
		if tx.LocalAmount != nil {
			row.InternalAmount = tx.LocalAmount.String()
		}
		if tx.LocalCurrency != nil {
			row.InternalCurrency = *tx.LocalCurrency
		}
		if tx.TxHash != "" {
			state, cached := chainCache[tx.TxHash]
			if !cached {
				ledger, lookupErr := client.TransactionDetail(tx.TxHash)
				switch {
				case stellar.IsNotFound(lookupErr):
					state = "missing"
				case lookupErr != nil:
					return nil, fmt.Errorf("query Horizon transaction %s: %w", tx.TxHash, lookupErr)
				case ledger.Hash != tx.TxHash:
					return nil, fmt.Errorf("Horizon returned a different transaction hash")
				case ledger.Successful:
					state = "settled"
				default:
					state = "failed"
				}
				chainCache[tx.TxHash] = state
			}
			row.ChainStatus = state
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reconcile Horizon: %w", err)
		}
		matches := byRef[ref]
		if len(matches) > 1 || counts[ref] > 1 || (tx.TxHash != "" && hashes[tx.TxHash] > 1) {
			row.Findings = append(row.Findings, "duplicate_reference_or_hash")
		}
		if len(matches) == 1 {
			b := matches[0]
			row.BankAmount = b.Amount.String()
			row.BankCurrency = b.Currency
			row.BankStatus = b.Status
			if tx.LocalAmount == nil || !tx.LocalAmount.Equal(b.Amount) {
				row.Findings = append(row.Findings, "amount_mismatch")
			}
			if tx.LocalCurrency == nil || *tx.LocalCurrency != b.Currency {
				row.Findings = append(row.Findings, "currency_mismatch")
			}
			if b.Status == "settled" && row.ChainStatus != "settled" {
				row.Findings = append(row.Findings, "fiat_settled_missing_on_chain")
			}
		}
		if row.ChainStatus == "settled" && (len(matches) == 0 || (len(matches) == 1 && matches[0].Status != "settled")) {
			row.Findings = append(row.Findings, "chain_settled_missing_in_fiat")
		}
		if row.ChainStatus == "settled" && tx.Status != domain.StatusConfirmed && tx.Status != domain.StatusSettled {
			row.Findings = append(row.Findings, "internal_status_mismatch")
		}
		if len(row.Findings) == 0 && (row.ChainStatus != "settled" || row.BankStatus != "settled") {
			row.Findings = append(row.Findings, "unsettled")
		}
		report.Rows = append(report.Rows, finishThreeWayRow(row))
	}
	for ref, entries := range byRef {
		if counts[ref] > 0 && len(entries) == 1 {
			continue
		}
		for _, b := range entries {
			row := ThreeWayRow{Reference: ref, BankAmount: b.Amount.String(), BankCurrency: b.Currency, BankStatus: b.Status, ChainStatus: "unknown", Findings: []string{"missing_internal_transfer"}}
			if counts[ref] > 0 {
				row.Findings = nil
			}
			if len(entries) > 1 {
				row.Findings = append(row.Findings, "duplicate_reference_or_hash")
			}
			report.Rows = append(report.Rows, finishThreeWayRow(row))
		}
	}
	sort.SliceStable(report.Rows, func(i, j int) bool {
		a, b := report.Rows[i], report.Rows[j]
		return a.Reference+"\x00"+a.TransferID+a.BankAmount < b.Reference+"\x00"+b.TransferID+b.BankAmount
	})
	return report, nil
}

func finishThreeWayRow(row ThreeWayRow) ThreeWayRow {
	if len(row.Findings) == 0 {
		row.Findings = []string{"matched"}
		row.Recommendation = "No action required"
		return row
	}
	recommendations := map[string]string{
		"duplicate_reference_or_hash":   "Resolve duplicate references/hashes before any retry",
		"amount_mismatch":               "Compare provider gross/net amounts, fees and stored fiat quote",
		"currency_mismatch":             "Verify provider currency and stored fiat quote; do not compare fiat to token units",
		"fiat_settled_missing_on_chain": "Verify provider settlement and Horizon history before investigating a safe retry",
		"chain_settled_missing_in_fiat": "Check provider cutoff, late reports and bank settlement; do not resubmit the chain payment",
		"internal_status_mismatch":      "Verify chain evidence and investigate internal settlement status",
		"missing_internal_transfer":     "Locate the internal transfer and its hash; chain status cannot be inferred from the bank reference",
		"unsettled":                     "Review pending/failed legs and settlement cutoff before retrying",
	}
	parts := make([]string, 0, len(row.Findings))
	for _, finding := range row.Findings {
		parts = append(parts, recommendations[finding])
	}
	row.Recommendation = strings.Join(parts, "; ")
	return row
}
