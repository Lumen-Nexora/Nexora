package stellar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPreflightReadsNeverSubmit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("broadcast method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/accounts/source":
			fmt.Fprint(w, `{"account_id":"source","sequence":"10","balances":[{"asset_type":"native","balance":"5"}]}`)
		case "/ledgers":
			if r.URL.Query().Get("order") != "desc" || r.URL.Query().Get("limit") != "1" {
				t.Error("not latest ledger")
			}
			fmt.Fprint(w, `{"_embedded":{"records":[{"sequence":99,"base_reserve_in_stroops":5000000,"base_fee_in_stroops":100}]}}`)
		case "/fee_stats":
			fmt.Fprint(w, `{"fee_charged":{"p95":"80"}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	c := NewPreflightClient(server.URL, time.Second)
	a, err := c.Account(context.Background(), "source")
	if err != nil || a.Sequence != "10" {
		t.Fatalf("%+v %v", a, err)
	}
	net, err := c.Network(context.Background())
	if err != nil || net.Fee != 100 || net.BaseReserve != 5000000 || net.Ledger != 99 || calls != 3 {
		t.Fatalf("%+v %v calls=%d", net, err, calls)
	}
}
func TestPreflightErrorsAndCancellation(t *testing.T) {
	for _, status := range []int{404, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			_, err := NewPreflightClient(server.URL, time.Second).Account(context.Background(), "source")
			if err == nil || errors.Is(err, ErrPreflightAccountMissing) != (status == 404) {
				t.Fatal(err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewPreflightClient(server.URL, time.Second).Account(ctx, "source")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestPreflightMalformedNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer server.Close()
	_, err := NewPreflightClient(server.URL, time.Second).Network(context.Background())
	if err == nil {
		t.Fatal("missing ledger accepted")
	}
}
