package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	horizonclient "github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/protocols/horizon"
)

// Provider fetches mid-market FX rates for a set of currency pairs.
// Swapping the implementation (e.g. Horizon → third-party oracle) requires
// only changing the concrete type passed to NewService — no handler changes.
type Provider interface {
	// GetRate returns the mid-market rate: units of `to` per one unit of `from`.
	GetRate(ctx context.Context, from, to, amount string) (decimal.Decimal, error)
	// SupportedPairs returns supported pairs in "FROM-TO" format.
	SupportedPairs() []string
}

// HorizonProvider fetches rates from the Stellar DEX order book via Horizon.
type HorizonProvider struct {
	client  *horizonclient.Client
	pairs   []string
	issuers map[string]string // asset code → issuer address; empty string for XLM
}

// NewHorizonProvider creates a Provider backed by the Stellar Horizon order book.
// pairs is a list of "FROM-TO" pair strings; issuers maps asset codes to their issuers.
func NewHorizonProvider(horizonURL string, pairs []string, issuers map[string]string) *HorizonProvider {
	return &HorizonProvider{
		client:  &horizonclient.Client{HorizonURL: horizonURL},
		pairs:   pairs,
		issuers: issuers,
	}
}

func (p *HorizonProvider) SupportedPairs() []string {
	return p.pairs
}

// GetRate queries the Horizon order book for the best ask price (from → to).
func (p *HorizonProvider) GetRate(ctx context.Context, from, to, _ string) (decimal.Decimal, error) {
	req := horizonclient.OrderBookRequest{
		SellingAssetType: horizonAssetType(from),
		BuyingAssetType:  horizonAssetType(to),
	}
	if from != "XLM" {
		req.SellingAssetCode = from
		req.SellingAssetIssuer = p.issuers[from]
	}
	if to != "XLM" {
		req.BuyingAssetCode = to
		req.BuyingAssetIssuer = p.issuers[to]
	}

	book, err := p.client.OrderBook(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("order book %s-%s: %w", from, to, err)
	}
	if len(book.Asks) == 0 {
		return decimal.Zero, fmt.Errorf("no liquidity for %s-%s", from, to)
	}

	rate, err := decimal.NewFromString(book.Asks[0].Price)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse ask price %q: %w", book.Asks[0].Price, err)
	}
	return rate, nil
}

func horizonAssetType(code string) horizonclient.AssetType {
	if code == "XLM" {
		return horizonclient.AssetTypeNative
	}
	if len(code) <= 4 {
		return horizonclient.AssetType4
	}
	return horizonclient.AssetType12
}

// GetDepth reads bids for selling from into to. Horizon expresses bid amounts
// in the counter (destination) asset, so divide by price for source capacity.
func (p *HorizonProvider) GetDepth(ctx context.Context, from, to string) ([]OrderbookLevel, error) {
	req := horizonclient.OrderBookRequest{SellingAssetType: horizonAssetType(from), BuyingAssetType: horizonAssetType(to), Limit: 200}
	if from != "XLM" {
		req.SellingAssetCode = from
		req.SellingAssetIssuer = p.issuers[from]
	}
	if to != "XLM" {
		req.BuyingAssetCode = to
		req.BuyingAssetIssuer = p.issuers[to]
	}
	request, err := req.HTTPRequest(strings.TrimRight(p.client.HorizonURL, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("build depth request: %w", err)
	}
	client := p.client.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(request.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("request depth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("depth request status: %d", resp.StatusCode)
	}
	var book horizon.OrderBookSummary
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		return nil, fmt.Errorf("decode depth: %w", err)
	}
	levels := make([]OrderbookLevel, 0, len(book.Bids))
	for _, bid := range book.Bids {
		price, err := decimal.NewFromString(bid.Price)
		if err != nil || price.Sign() <= 0 {
			return nil, fmt.Errorf("invalid depth price %q", bid.Price)
		}
		amount, err := decimal.NewFromString(bid.Amount)
		if err != nil || amount.Sign() <= 0 {
			return nil, fmt.Errorf("invalid depth amount %q", bid.Amount)
		}
		levels = append(levels, OrderbookLevel{Price: price, Amount: amount.Div(price)})
	}
	return levels, nil
}
