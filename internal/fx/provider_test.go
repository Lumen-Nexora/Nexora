package fx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"
)

func TestHorizonDepthSourceUnits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/order_book" || r.URL.Query().Get("selling_asset_type") != "native" || r.URL.Query().Get("buying_asset_code") != "USDC" {
			t.Errorf("unexpected URL %s", r.URL)
		}
		fmt.Fprint(w, `{"bids":[{"price":"2","amount":"200"},{"price":"1.9","amount":"190"}],"asks":[{"price":"3","amount":"10000"}]}`)
	}))
	defer server.Close()
	provider := NewHorizonProvider(server.URL, []string{"XLM-USDC"}, map[string]string{"USDC": "issuer"})
	levels, err := provider.GetDepth(context.Background(), "XLM", "USDC")
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 || !levels[0].Amount.Equal(decimal.NewFromInt(100)) || !levels[1].Amount.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("wrong source capacity: %+v", levels)
	}
	_, _, pct, _, err := CalculatePriceImpact(decimal.NewFromInt(200), levels)
	if err != nil || !pct.Equal(decimal.RequireFromString("2.5")) {
		t.Fatalf("impact=%s err=%v", pct, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.GetDepth(ctx, "XLM", "USDC"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestHorizonDepthFailsClosed(t *testing.T) {
	for _, body := range []string{`{"bids":[]}`, `{"bids":[{"price":"0","amount":"1"}]}`, `{"bids":[{"price":"1","amount":"bad"}]}`, `invalid`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			p := NewHorizonProvider(server.URL, nil, nil)
			levels, err := p.GetDepth(context.Background(), "XLM", "USDC")
			if err == nil {
				_, err = NewSlippageGuard().Check(decimal.NewFromInt(1), nil, levels)
			}
			if err == nil {
				t.Fatal("invalid or empty depth allowed trade")
			}
		})
	}
}
