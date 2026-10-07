package wallet_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/wallet"
	"github.com/go-chi/chi/v5"
)

// newWalletTestRouter mounts the real Routes() so tests exercise the actual
// HTTP surface (status codes), not just the service layer directly.
func newWalletTestRouter(t *testing.T, svc wallet.Service) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	h := wallet.NewHandler(svc)
	r.Route("/wallets", func(r chi.Router) { h.Routes()(r) })
	return r
}

// TestCreateWallet_MalformedBodyIsRejected is the regression test for #246:
// a request body that isn't valid JSON must not silently fall through to
// wallet creation with zero-value fields.
func TestCreateWallet_MalformedBodyIsRejected(t *testing.T) {
	svc := &fakeContractService{}
	h := newWalletTestRouter(t, svc)

	req := httptest.NewRequest(http.MethodPost, "/wallets/", strings.NewReader("{not valid json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateWallet_EmptyBodyStillSucceeds ensures the fix doesn't regress the
// legitimate case: no body at all is a valid way to request a default
// custodial wallet, and must still succeed.
func TestCreateWallet_EmptyBodyStillSucceeds(t *testing.T) {
	svc := &fakeContractService{}
	h := newWalletTestRouter(t, svc)

	req := httptest.NewRequest(http.MethodPost, "/wallets/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for empty body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateWallet_ValidBodyIsParsed confirms well-formed requests are
// unaffected by the fix.
func TestCreateWallet_ValidBodyIsParsed(t *testing.T) {
	svc := &fakeContractService{}
	h := newWalletTestRouter(t, svc)

	req := httptest.NewRequest(http.MethodPost, "/wallets/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for valid body, got %d: %s", rec.Code, rec.Body.String())
	}
}
