package wallet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stellar/go/keypair"
)

type simulationReader struct {
	accounts map[string]stellar.PreflightAccount
	calls    int
	err      error
}

func (r *simulationReader) Account(ctx context.Context, id string) (stellar.PreflightAccount, error) {
	r.calls++
	if r.err != nil {
		return stellar.PreflightAccount{}, r.err
	}
	a, ok := r.accounts[id]
	if !ok {
		return a, stellar.ErrPreflightAccountMissing
	}
	return a, nil
}
func (r *simulationReader) Network(context.Context) (stellar.PreflightNetwork, error) {
	r.calls++
	return stellar.PreflightNetwork{Ledger: 123, Fee: 100, BaseReserve: 5000000}, r.err
}
func simulationFixture(t testing.TB) (*balanceTestRepository, *simulationReader, SimulationRequest, context.Context) {
	t.Helper()
	source := keypair.MustRandom().Address()
	dest := keypair.MustRandom().Address()
	repo := &balanceTestRepository{wallet: &domain.Wallet{ID: "w", PublicKey: source, Mode: domain.ModeTest}}
	reader := &simulationReader{accounts: map[string]stellar.PreflightAccount{
		source: {AccountID: source, Sequence: "123", Balances: []stellar.PreflightBalance{{Type: "native", Balance: "10.0000000"}}},
		dest:   {AccountID: dest, Sequence: "4", Balances: []stellar.PreflightBalance{{Type: "native", Balance: "2"}}},
	}}
	return repo, reader, SimulationRequest{Destination: dest, Asset: "XLM", Amount: "1"}, tenant.WithMode(context.Background(), domain.ModeTest)
}
func TestSimulationPaymentChecks(t *testing.T) {
	for _, test := range []struct {
		name, code string
		change     func(*balanceTestRepository, *simulationReader, *SimulationRequest)
	}{
		{"success", "ok", func(_ *balanceTestRepository, _ *simulationReader, _ *SimulationRequest) {}},
		{"reserve and fee", "insufficient_native_balance", func(_ *balanceTestRepository, _ *simulationReader, q *SimulationRequest) { q.Amount = "9" }},
		{"exact spendable boundary", "ok", func(_ *balanceTestRepository, _ *simulationReader, q *SimulationRequest) { q.Amount = "8.9999900" }},
		{"sequence mismatch", "bad_sequence", func(_ *balanceTestRepository, _ *simulationReader, q *SimulationRequest) { q.Sequence = "123" }},
		{"destination missing", "destination_missing", func(_ *balanceTestRepository, r *simulationReader, q *SimulationRequest) {
			delete(r.accounts, q.Destination)
		}},
		{"source missing", "source_missing", func(w *balanceTestRepository, r *simulationReader, _ *SimulationRequest) {
			delete(r.accounts, w.wallet.PublicKey)
		}},
		{"selling liabilities", "insufficient_native_balance", func(w *balanceTestRepository, r *simulationReader, q *SimulationRequest) {
			a := r.accounts[w.wallet.PublicKey]
			a.Balances[0].Selling = "8"
			r.accounts[w.wallet.PublicKey] = a
		}},
		{"sponsoring raises reserve", "insufficient_native_balance", func(w *balanceTestRepository, r *simulationReader, q *SimulationRequest) {
			a := r.accounts[w.wallet.PublicKey]
			a.NumSponsoring = 16
			r.accounts[w.wallet.PublicKey] = a
		}},
		{"sponsored base reserve", "ok", func(w *balanceTestRepository, r *simulationReader, q *SimulationRequest) {
			a := r.accounts[w.wallet.PublicKey]
			a.NumSponsored = 2
			r.accounts[w.wallet.PublicKey] = a
			q.Amount = "9.9999900"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, r, q, ctx := simulationFixture(t)
			test.change(w, r, &q)
			got, err := NewSimulationService(w, r, r, nil, "test").Simulate(ctx, "w", q)
			if err != nil {
				t.Fatal(err)
			}
			if got.Code != test.code || got.Success != (test.code == "ok") {
				t.Fatalf("result=%+v", got)
			}
			if got.EstimatedFee != "100" {
				t.Fatal(got)
			}
		})
	}
}
func TestSimulationCreditTrustlines(t *testing.T) {
	for _, test := range []struct {
		name, code string
		edit       func(*stellar.PreflightAccount, *stellar.PreflightAccount)
	}{
		{"success", "ok", func(s, d *stellar.PreflightAccount) {}},
		{"source missing", "source_trustline_missing", func(s, d *stellar.PreflightAccount) { s.Balances = s.Balances[:1] }},
		{"source unauthorized", "source_trustline_unauthorized", func(s, d *stellar.PreflightAccount) { s.Balances[1].Authorized = false }},
		{"source liabilities", "insufficient_asset_balance", func(s, d *stellar.PreflightAccount) { s.Balances[1].Selling = "10" }},
		{"destination missing", "destination_trustline_missing", func(s, d *stellar.PreflightAccount) { d.Balances = d.Balances[:1] }},
		{"destination unauthorized", "destination_trustline_unauthorized", func(s, d *stellar.PreflightAccount) { d.Balances[1].Authorized = false }},
		{"destination liabilities", "destination_capacity_exceeded", func(s, d *stellar.PreflightAccount) { d.Balances[1].Buying = "9.1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, r, q, ctx := simulationFixture(t)
			q.Asset = "USD"
			q.Issuer = keypair.MustRandom().Address()
			s := r.accounts[w.wallet.PublicKey]
			d := r.accounts[q.Destination]
			b := stellar.PreflightBalance{Type: "credit_alphanum4", Code: "USD", Issuer: q.Issuer, Balance: "10", Limit: "20", Authorized: true}
			s.Balances = append(s.Balances, b)
			d.Balances = append(d.Balances, b)
			test.edit(&s, &d)
			r.accounts[w.wallet.PublicKey] = s
			r.accounts[q.Destination] = d
			got, err := NewSimulationService(w, r, r, nil, "test").Simulate(ctx, "w", q)
			if err != nil || got.Code != test.code {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}
func TestSimulationValidationAndIsolation(t *testing.T) {
	for _, amount := range []string{"0", "-1", "1.00000001", "1e100", "922337203685.4775808", "NaN"} {
		t.Run(amount, func(t *testing.T) {
			w, r, q, ctx := simulationFixture(t)
			q.Amount = amount
			_, err := NewSimulationService(w, r, r, nil, "test").Simulate(ctx, "w", q)
			if !errors.Is(err, domain.ErrInvalidAmount) || r.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, r.calls)
			}
		})
	}
	w, r, q, ctx := simulationFixture(t)
	svc := NewSimulationService(w, r, r, nil, "test")
	_, err := svc.Simulate(context.Background(), "w", q)
	if !errors.Is(err, domain.ErrWalletNotFound) || r.calls != 0 {
		t.Fatalf("environment leak: %v", err)
	}
	owner := "other"
	w.wallet.TenantID = &owner
	_, err = svc.Simulate(ctx, "w", q)
	if !errors.Is(err, domain.ErrWalletNotFound) || r.calls != 0 {
		t.Fatalf("tenant leak: %v", err)
	}
	w.wallet.TenantID = nil
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = svc.Simulate(cancelCtx, "w", q)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	w.wallet.CustodyType = domain.CustodyContract
	_, err = svc.Simulate(ctx, "w", q)
	if !errors.Is(err, domain.ErrInvalidAsset) {
		t.Fatal(err)
	}
}
func TestSimulationCacheExpiryAndFailure(t *testing.T) {
	w, r, q, ctx := simulationFixture(t)
	mr := miniredis.RunT(t)
	cache := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	defer cache.Close()
	svc := NewSimulationService(w, r, r, cache, "network-one")
	first, err := svc.Simulate(ctx, "w", q)
	if err != nil || first.Cached {
		t.Fatalf("%+v %v", first, err)
	}
	calls := r.calls
	second, err := svc.Simulate(ctx, "w", q)
	if err != nil || !second.Cached || r.calls != calls {
		t.Fatalf("%+v %v calls=%d", second, err, r.calls)
	}
	// A different endpoint namespace must never reuse another network's state.
	_, err = NewSimulationService(w, r, r, cache, "network-two").Simulate(ctx, "w", q)
	if err != nil || r.calls == calls {
		t.Fatal("network cache collision", err)
	}
	a := r.accounts[w.wallet.PublicKey]
	a.Sequence = "124"
	a.Balances[0].Balance = "1"
	r.accounts[w.wallet.PublicKey] = a
	mr.FastForward(3 * time.Second)
	third, err := svc.Simulate(ctx, "w", q)
	if err != nil || third.Success || third.Sequence != "125" {
		t.Fatalf("stale result=%+v %v", third, err)
	}
	mr.Close()
	r.err = errors.New("horizon down")
	_, err = svc.Simulate(ctx, "w", q)
	if err == nil {
		t.Fatal("upstream failure reported success")
	}
}
func TestSimulationHandler(t *testing.T) {
	w, r, q, ctx := simulationFixture(t)
	h := NewHandler(nil).WithSimulation(NewSimulationService(w, r, r, nil, "test"))
	router := chi.NewRouter()
	router.Route("/wallets", h.Routes())
	for _, body := range []string{fmt.Sprintf(`{"destination":%q,"amount":"1","asset":"XLM"}`, q.Destination), `{"amount":"1","unknown":true}`, `{} {}`} {
		request := httptest.NewRequest(http.MethodPost, "/wallets/w/simulate", strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, request)
		want := http.StatusBadRequest
		if strings.Contains(body, q.Destination) {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
	}
}
func BenchmarkSimulationCached(b *testing.B) {
	w, r, q, ctx := simulationFixture(b)
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	defer mr.Close()
	cache := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cache.Close()
	svc := NewSimulationService(w, r, r, cache, "benchmark")
	if _, err = svc.Simulate(ctx, "w", q); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err = svc.Simulate(ctx, "w", q); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSimulationIssuerExemptionsAndMalformedState(t *testing.T) {
	for _, sourceIssuer := range []bool{true, false} {
		t.Run(fmt.Sprint(sourceIssuer), func(t *testing.T) {
			w, r, q, ctx := simulationFixture(t)
			q.Asset = "USD"
			q.Issuer = q.Destination
			if sourceIssuer {
				q.Issuer = w.wallet.PublicKey
			}
			holder := w.wallet.PublicKey
			if sourceIssuer {
				holder = q.Destination
			}
			a := r.accounts[holder]
			a.Balances = append(a.Balances, stellar.PreflightBalance{Type: "credit_alphanum4", Code: "USD", Issuer: q.Issuer, Balance: "5", Limit: "10", Authorized: true})
			r.accounts[holder] = a
			got, err := NewSimulationService(w, r, r, nil, "test").Simulate(ctx, "w", q)
			if err != nil || !got.Success {
				t.Fatalf("issuer exemption: %+v %v", got, err)
			}
		})
	}
	w, r, q, ctx := simulationFixture(t)
	a := r.accounts[w.wallet.PublicKey]
	a.Balances[0].Balance = "bad"
	r.accounts[w.wallet.PublicKey] = a
	_, err := NewSimulationService(w, r, r, nil, "test").Simulate(ctx, "w", q)
	if err == nil {
		t.Fatal("malformed balance accepted")
	}
}
