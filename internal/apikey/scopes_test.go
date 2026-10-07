package apikey

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
)

func TestBatchAndReportScopes(t *testing.T) {
	for _, s := range []string{
		domain.ScopeBatchesRead, domain.ScopeBatchesWrite, "batches:*",
		domain.ScopeReportsRead, domain.ScopeReportsWrite, "reports:*",
	} {
		if err := domain.ValidateScopes([]string{s}); err != nil {
			t.Errorf("scope %q should be valid: %v", s, err)
		}
	}

	if !domain.HasScope([]string{"batches:*"}, domain.ScopeBatchesWrite) {
		t.Error("batches:* should allow batches:write")
	}
	if domain.HasScope([]string{domain.ScopeBatchesWrite}, domain.ScopeTransfersWrite) {
		t.Error("batches:write must not allow single transfers")
	}
	if domain.HasScope([]string{domain.ScopeReportsRead}, domain.ScopeReportsWrite) {
		t.Error("reports:read must not allow reports:write")
	}
}

func TestLegacyTransferScopesStillGrantBatches(t *testing.T) {
	// Batches were gated by transfers:write before batches:* existed.
	if !domain.HasScope([]string{domain.ScopeTransfersWrite}, domain.ScopeBatchesWrite) {
		t.Error("transfers:write should keep granting batches:write")
	}
	if !domain.HasScope([]string{domain.ScopeTransfersRead}, domain.ScopeBatchesRead) {
		t.Error("transfers:read should keep granting batches:read")
	}
	if !domain.HasScope([]string{"transfers:*"}, domain.ScopeBatchesWrite) {
		t.Error("transfers:* should keep granting batches:write")
	}
	if domain.HasScope([]string{domain.ScopeTransfersRead}, domain.ScopeBatchesWrite) {
		t.Error("transfers:read must not grant batches:write")
	}
}

func scopedKeyRouter(h *Handler, callerScopes []string) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := tenant.WithID(req.Context(), "tenant-1")
			ctx = tenant.WithMode(ctx, domain.ModeLive)
			if callerScopes == nil {
				ctx = tenant.WithUser(ctx, "user-1", domain.RoleOwner)
			} else {
				ctx = tenant.WithUser(ctx, "", domain.RoleAdmin)
				ctx = tenant.WithScopes(ctx, callerScopes)
				ctx = tenant.WithAPIKeyID(ctx, "caller-key")
			}
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Post("/keys", h.Create)
	r.Get("/keys", h.List)
	return r
}

func createKey(t *testing.T, r http.Handler, scopes interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{"label": "integration"}
	if scopes != nil {
		body["scopes"] = scopes
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(raw)))
	return w
}

func TestCreateKeyWithScopesAndListWithoutSecret(t *testing.T) {
	repo := newMockKeyRepo()
	r := scopedKeyRouter(NewHandler(repo), nil)
	var rawKeys []string

	for name, scopes := range map[string][]string{
		"read-only":  {domain.ScopeWalletsRead, domain.ScopeTransfersRead},
		"write-only": {domain.ScopeTransfersWrite},
		"mixed":      {domain.ScopeWalletsRead, domain.ScopeBatchesWrite, domain.ScopeReportsRead},
	} {
		t.Run(name, func(t *testing.T) {
			w := createKey(t, r, scopes)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var created struct {
				ID     string   `json:"id"`
				Key    string   `json:"key"`
				Scopes []string `json:"scopes"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &created)
			if created.Key == "" || strings.Join(created.Scopes, ",") != strings.Join(scopes, ",") {
				t.Fatalf("unexpected create response: %s", w.Body.String())
			}
			rawKeys = append(rawKeys, created.Key)
			if got := repo.keys[created.ID].Scopes; strings.Join(got, ",") != strings.Join(scopes, ",") {
				t.Fatalf("persisted scopes = %v, want %v", got, scopes)
			}
		})
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"scopes"`) || !strings.Contains(body, domain.ScopeBatchesWrite) {
		t.Fatalf("list should expose scopes: %s", body)
	}
	// The prefix (e.g. sk_live_AbCd1234) is public; the full key never is.
	for _, secret := range append([]string{`"key"`, "key_hash"}, rawKeys...) {
		if strings.Contains(body, secret) {
			t.Fatalf("list response leaks %q: %s", secret, body)
		}
	}
}

func TestCreateKeyRejectsInvalidScopes(t *testing.T) {
	r := scopedKeyRouter(NewHandler(newMockKeyRepo()), nil)

	for _, scopes := range [][]string{
		{"wallets:delete"},
		{domain.ScopeWalletsRead, "not-a-scope"},
		{""},
		{"reports"},
	} {
		if w := createKey(t, r, scopes); w.Code != http.StatusBadRequest {
			t.Errorf("scopes %v: status = %d, want 400", scopes, w.Code)
		}
	}
}

func TestScopedKeyCannotMintBroaderKey(t *testing.T) {
	caller := []string{domain.ScopeKeysWrite, domain.ScopeTransfersRead}
	r := scopedKeyRouter(NewHandler(newMockKeyRepo()), caller)

	for name, scopes := range map[string]interface{}{
		"full access (no scopes)": nil,
		"wildcard":                []string{domain.ScopeWildcard},
		"write it lacks":          []string{domain.ScopeTransfersWrite},
		"resource wildcard":       []string{"transfers:*"},
	} {
		t.Run(name, func(t *testing.T) {
			w := createKey(t, r, scopes)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "INSUFFICIENT_SCOPE") {
				t.Fatalf("status = %d body = %s, want 403 INSUFFICIENT_SCOPE", w.Code, w.Body.String())
			}
		})
	}

	if w := createKey(t, r, []string{domain.ScopeTransfersRead}); w.Code != http.StatusCreated {
		t.Fatalf("narrower key: status = %d: %s", w.Code, w.Body.String())
	}
}
