package fiat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
)

// stubService lets a handler test dictate what the service returns.
type stubService struct {
	depositErr  error
	withdrawErr error
}

func (s *stubService) GetQuote(context.Context, QuoteRequest) (*FiatQuote, error) {
	return &FiatQuote{}, nil
}

func (s *stubService) InitiateDeposit(context.Context, DepositRequest) (*DepositResponse, error) {
	return nil, s.depositErr
}

func (s *stubService) InitiateWithdrawal(context.Context, WithdrawRequest) (*WithdrawResponse, error) {
	return nil, s.withdrawErr
}

func (s *stubService) HandleWebhook(context.Context, []byte, string) error { return nil }

func (s *stubService) HandleWebhookWithHeaders(context.Context, []byte, http.Header) error {
	return nil
}

func newFiatRouter(svc Service) http.Handler {
	h := NewHandler(svc)
	r := chi.NewRouter()
	r.Route("/wallets/{id}/deposit", h.DepositRoutes())
	r.Route("/wallets/{id}/withdraw", h.WithdrawRoutes())
	return r
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestHandler_UnsupportedFiatCurrency_HasDistinctCode(t *testing.T) {
	unsupported := errors.Join(errors.New("wrapped"), domain.ErrUnsupportedFiatCurrency)

	cases := []struct {
		name string
		path string
		body string
		svc  *stubService
	}{
		{
			name: "deposit",
			path: "/wallets/w1/deposit/fiat",
			body: `{"amount":"100","currency":"XYZ","email":"a@b.co","name":"A"}`,
			svc:  &stubService{depositErr: unsupported},
		},
		{
			name: "withdrawal",
			path: "/wallets/w1/withdraw/fiat",
			body: `{"amount":"100","currency":"XYZ","account_bank":"044","account_number":"0123456789"}`,
			svc:  &stubService{withdrawErr: unsupported},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			newFiatRouter(tc.svc).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := errorCode(t, rec); got != "UNSUPPORTED_FIAT_CURRENCY" {
				t.Errorf("expected code UNSUPPORTED_FIAT_CURRENCY, got %q", got)
			}
		})
	}
}

func TestHandler_OtherServiceErrors_StayInternal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/wallets/w1/deposit/fiat",
		strings.NewReader(`{"amount":"100","currency":"NGN","email":"a@b.co","name":"A"}`))
	rec := httptest.NewRecorder()
	newFiatRouter(&stubService{depositErr: errors.New("rail down")}).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR, got %q", got)
	}
}
