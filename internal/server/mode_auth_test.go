package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
)

func TestRequireTestModeRejectsLiveEnvironment(t *testing.T) {
	called := false
	h := RequireTestMode(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	ctx := tenant.WithID(t.Context(), "tenant-1")
	ctx = tenant.WithMode(ctx, domain.ModeLive)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/test/trigger-event", nil).WithContext(ctx))
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("live request = %d, called=%v", rec.Code, called)
	}
}

func TestRequireTestModeAllowsTestEnvironment(t *testing.T) {
	called := false
	h := RequireTestMode(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	ctx := tenant.WithID(t.Context(), "tenant-1")
	ctx = tenant.WithMode(ctx, domain.ModeTest)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/test/trigger-event", nil).WithContext(ctx))
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("test request = %d, called=%v", rec.Code, called)
	}
}
