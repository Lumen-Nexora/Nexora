package fees

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

const (
	defaultCoinGeckoHistoryURL = "https://api.coingecko.com/api/v3/coins/stellar/market_chart/range"
	networkFeeCostCenter       = "blockchain_network"
	historicalRateTimeout      = 8 * time.Second
	usdPrecision               = int32(8)
)

var (
	// ErrInvalidNetworkFee rejects a network-fee event that cannot be attributed
	// or valued consistently.
	ErrInvalidNetworkFee = errors.New("invalid network fee charge")
)

// HistoricalRateSource resolves the historical rate of one asset in another at
// a specific instant. Implementations must never substitute a current rate for
// a missing historical quote: returning an error leaves the cost explicitly
// unvalued so it can be backfilled later.
type HistoricalRateSource interface {
	RateAt(ctx context.Context, from, to string, at time.Time) (decimal.Decimal, error)
}

// NetworkFeeEvent is the exact on-chain fee information extracted from a
// Horizon transaction result.
type NetworkFeeEvent struct {
	TransactionID     string
	TxHash            string
	TenantID          *string
	SubAccount        string
	CostCenter        string
	Allocation        domain.NetworkFeeAllocation
	LedgerSequence    int32
	LedgerClosedAt    time.Time
	FeeChargedStroops int64
}

// NetworkFeeAccountingService captures network fees, reports tenant consumption
// and produces monthly cost-of-goods-sold reports.
type NetworkFeeAccountingService interface {
	CaptureNetworkFee(ctx context.Context, event NetworkFeeEvent) (*domain.NetworkFeeCharge, error)
	GetNetworkFeeConsumption(ctx context.Context, tenantID string, start, end time.Time) (*domain.NetworkFeeUsage, error)
	GetMonthlyNetworkFeeCOGS(ctx context.Context, month time.Time) (*domain.NetworkFeeCOGSReport, error)
}

type networkFeeAccounting struct {
	repo  Repository
	rates HistoricalRateSource
}

// NewNetworkFeeAccounting builds the network-fee accounting engine. A nil rate
// source is allowed: charges are still recorded, but remain explicitly
// unvalued until a historical quote is available.
func NewNetworkFeeAccounting(repo Repository, rates HistoricalRateSource) NetworkFeeAccountingService {
	return &networkFeeAccounting{repo: repo, rates: rates}
}

func (a *networkFeeAccounting) CaptureNetworkFee(ctx context.Context, event NetworkFeeEvent) (*domain.NetworkFeeCharge, error) {
	if err := event.validate(); err != nil {
		return nil, err
	}

	existing, err := a.repo.GetNetworkFeeByTxHash(ctx, event.TxHash)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, domain.ErrNetworkFeeNotFound) {
		return nil, fmt.Errorf("look up network fee: %w", err)
	}

	tenantID := normalizedTenantID(event.TenantID)
	allocation := event.normalizedAllocation(tenantID)
	costCenter := strings.TrimSpace(event.CostCenter)
	if costCenter == "" {
		costCenter = networkFeeCostCenter
	}

	charge := &domain.NetworkFeeCharge{
		ID:                uuid.New().String(),
		TransactionID:     optionalString(event.TransactionID),
		TxHash:            event.TxHash,
		TenantID:          tenantID,
		SubAccount:        strings.TrimSpace(event.SubAccount),
		CostCenter:        costCenter,
		Allocation:        allocation,
		LedgerSequence:    event.LedgerSequence,
		LedgerClosedAt:    event.LedgerClosedAt.UTC(),
		FeeChargedStroops: event.FeeChargedStroops,
		XLMAmount:         StroopsToXLM(event.FeeChargedStroops),
		CreatedAt:         time.Now().UTC(),
	}

	if a.rates != nil {
		rate, rateErr := a.rates.RateAt(ctx, "XLM", "USD", event.LedgerClosedAt)
		switch {
		case rateErr != nil:
			log.Warn().Err(rateErr).
				Str("tx_hash", event.TxHash).
				Int32("ledger", event.LedgerSequence).
				Msg("network fee left unvalued: historical XLM/USD rate unavailable")
		case !rate.IsPositive():
			log.Warn().
				Str("tx_hash", event.TxHash).
				Int32("ledger", event.LedgerSequence).
				Str("rate", rate.String()).
				Msg("network fee left unvalued: historical XLM/USD rate is not positive")
		default:
			usd := charge.XLMAmount.Mul(rate).Round(usdPrecision)
			charge.XLMUSDRate = &rate
			charge.USDAmount = &usd
			charge.Valued = true
		}
	}

	if err := a.repo.RecordNetworkFee(ctx, charge); err != nil {
		return nil, err
	}
	return charge, nil
}

func (a *networkFeeAccounting) GetNetworkFeeConsumption(ctx context.Context, tenantID string, start, end time.Time) (*domain.NetworkFeeUsage, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidNetworkFee)
	}

	start, end = normalizePeriod(start, end)
	if !end.After(start) {
		return nil, fmt.Errorf("%w: period end must be after period start", ErrInvalidNetworkFee)
	}

	usage, err := a.repo.GetNetworkFeeUsage(ctx, tenantID, start, end)
	if err != nil {
		return nil, err
	}
	usage.TenantID = tenantID
	usage.PeriodStart = start
	usage.PeriodEnd = end
	return usage, nil
}

func (a *networkFeeAccounting) GetMonthlyNetworkFeeCOGS(ctx context.Context, month time.Time) (*domain.NetworkFeeCOGSReport, error) {
	start := monthStart(month)
	end := start.AddDate(0, 1, 0)

	report, err := a.repo.GetMonthlyNetworkFeeCOGS(ctx, start, end)
	if err != nil {
		return nil, err
	}
	report.Month = start.Format("2006-01")
	report.PeriodStart = start
	report.PeriodEnd = end
	report.Allocations = allocationTotals(report.Lines)
	return report, nil
}

func (e NetworkFeeEvent) validate() error {
	if strings.TrimSpace(e.TxHash) == "" {
		return fmt.Errorf("%w: tx_hash is required", ErrInvalidNetworkFee)
	}
	if e.FeeChargedStroops < 0 {
		return fmt.Errorf("%w: fee_charged must not be negative", ErrInvalidNetworkFee)
	}
	if e.LedgerSequence < 0 {
		return fmt.Errorf("%w: ledger_sequence must not be negative", ErrInvalidNetworkFee)
	}
	if e.LedgerClosedAt.IsZero() {
		return fmt.Errorf("%w: ledger close time is required", ErrInvalidNetworkFee)
	}

	tenantID := normalizedTenantID(e.TenantID)
	switch e.Allocation {
	case "":
		return nil
	case domain.NetworkFeeAllocationTenant:
		if tenantID == nil {
			return fmt.Errorf("%w: tenant allocation requires a tenant_id", ErrInvalidNetworkFee)
		}
	case domain.NetworkFeeAllocationOverhead:
		return nil
	default:
		return fmt.Errorf("%w: unknown allocation %q", ErrInvalidNetworkFee, e.Allocation)
	}
	return nil
}

func (e NetworkFeeEvent) normalizedAllocation(tenantID *string) domain.NetworkFeeAllocation {
	if e.Allocation != "" {
		return e.Allocation
	}
	if tenantID != nil {
		return domain.NetworkFeeAllocationTenant
	}
	return domain.NetworkFeeAllocationOverhead
}

// NetworkFeeSettlementRecorder adapts the accounting engine to the settlement
// poller's details-recorder contract.
type NetworkFeeSettlementRecorder struct {
	accounting NetworkFeeAccountingService
}

func NewNetworkFeeSettlementRecorder(accounting NetworkFeeAccountingService) *NetworkFeeSettlementRecorder {
	return &NetworkFeeSettlementRecorder{accounting: accounting}
}

func (r *NetworkFeeSettlementRecorder) RecordSettlementDetails(
	ctx context.Context,
	txID, txHash string,
	tenantID *string,
	ledgerSequence int32,
	feeCharged int64,
	ledgerClosedAt time.Time,
) error {
	_, err := r.accounting.CaptureNetworkFee(ctx, NetworkFeeEvent{
		TransactionID:     txID,
		TxHash:            txHash,
		TenantID:          tenantID,
		CostCenter:        networkFeeCostCenter,
		LedgerSequence:    ledgerSequence,
		LedgerClosedAt:    ledgerClosedAt,
		FeeChargedStroops: feeCharged,
	})
	return err
}

// CoinGeckoHistoricalRateSource resolves historical XLM/USD prices from
// CoinGecko's market-chart range endpoint and selects the latest observation at
// or before the requested ledger close time.
type CoinGeckoHistoricalRateSource struct {
	baseURL string
	client  *http.Client

	mu    sync.Mutex
	cache map[int64]decimal.Decimal
}

func NewCoinGeckoHistoricalRateSource() *CoinGeckoHistoricalRateSource {
	return NewCoinGeckoHistoricalRateSourceWithClient(defaultCoinGeckoHistoryURL, &http.Client{Timeout: historicalRateTimeout})
}

// NewCoinGeckoHistoricalRateSourceWithClient is used by tests to inject a mock
// endpoint and HTTP client.
func NewCoinGeckoHistoricalRateSourceWithClient(baseURL string, client *http.Client) *CoinGeckoHistoricalRateSource {
	if client == nil {
		client = &http.Client{Timeout: historicalRateTimeout}
	}
	return &CoinGeckoHistoricalRateSource{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
		cache:   make(map[int64]decimal.Decimal),
	}
}

func (s *CoinGeckoHistoricalRateSource) RateAt(ctx context.Context, from, to string, at time.Time) (decimal.Decimal, error) {
	if !strings.EqualFold(from, "XLM") || !strings.EqualFold(to, "USD") {
		return decimal.Zero, fmt.Errorf("historical rate source only supports XLM/USD, got %s/%s", from, to)
	}
	if at.IsZero() {
		return decimal.Zero, errors.New("historical rate requires a timestamp")
	}

	cacheKey := at.UTC().Truncate(time.Minute).Unix()
	s.mu.Lock()
	cached, ok := s.cache[cacheKey]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}

	rate, err := s.fetch(ctx, at)
	if err != nil {
		return decimal.Zero, err
	}

	s.mu.Lock()
	if len(s.cache) >= 4096 {
		s.cache = make(map[int64]decimal.Decimal)
	}
	s.cache[cacheKey] = rate
	s.mu.Unlock()
	return rate, nil
}

type coinGeckoHistoryResponse struct {
	Prices [][]float64 `json:"prices"`
}

func (s *CoinGeckoHistoricalRateSource) fetch(ctx context.Context, at time.Time) (decimal.Decimal, error) {
	query := url.Values{}
	query.Set("vs_currency", "usd")
	query.Set("from", strconv.FormatInt(at.Add(-time.Hour).Unix(), 10))
	query.Set("to", strconv.FormatInt(at.Add(time.Hour).Unix(), 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"?"+query.Encode(), nil)
	if err != nil {
		return decimal.Zero, fmt.Errorf("historical rate request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nexora-network-fee-accounting/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("historical rate fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, fmt.Errorf("historical rate status %d", resp.StatusCode)
	}

	var body coinGeckoHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return decimal.Zero, fmt.Errorf("historical rate decode: %w", err)
	}

	target := at.UTC().Unix()
	bestTimestamp := int64(-1)
	bestPrice := 0.0
	for _, point := range body.Prices {
		if len(point) < 2 {
			continue
		}
		ts := int64(point[0] / 1000)
		if ts > target || point[1] <= 0 {
			continue
		}
		if ts > bestTimestamp {
			bestTimestamp = ts
			bestPrice = point[1]
		}
	}
	if bestTimestamp < 0 {
		return decimal.Zero, fmt.Errorf("historical rate unavailable for XLM/USD at %s", at.UTC().Format(time.RFC3339))
	}

	rate, err := decimal.NewFromString(strconv.FormatFloat(bestPrice, 'f', -1, 64))
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse historical rate: %w", err)
	}
	return rate, nil
}

func normalizedTenantID(tenantID *string) *string {
	if tenantID == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*tenantID)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func normalizePeriod(start, end time.Time) (time.Time, time.Time) {
	now := time.Now().UTC()
	if end.IsZero() {
		end = now
	} else {
		end = end.UTC()
	}
	if start.IsZero() {
		start = time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		start = start.UTC()
	}
	return start, end
}

func monthStart(month time.Time) time.Time {
	if month.IsZero() {
		month = time.Now().UTC()
	}
	month = month.UTC()
	return time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func allocationTotals(lines []domain.NetworkFeeCOGSLine) []domain.NetworkFeeAllocationTotal {
	totals := make(map[domain.NetworkFeeAllocation]*domain.NetworkFeeAllocationTotal)
	order := make([]domain.NetworkFeeAllocation, 0, 2)
	for _, line := range lines {
		total, ok := totals[line.Allocation]
		if !ok {
			total = &domain.NetworkFeeAllocationTotal{Allocation: line.Allocation}
			totals[line.Allocation] = total
			order = append(order, line.Allocation)
		}
		total.TransactionCount += line.TransactionCount
		total.FeeChargedStroops += line.FeeChargedStroops
		total.XLMAmount = total.XLMAmount.Add(line.XLMAmount)
		total.USDAmount = total.USDAmount.Add(line.USDAmount)
		total.UnvaluedCount += line.UnvaluedCount
	}

	out := make([]domain.NetworkFeeAllocationTotal, 0, len(order))
	for _, allocation := range order {
		out = append(out, *totals[allocation])
	}
	return out
}
