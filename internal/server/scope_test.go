package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
)

// scopeTestRouter mirrors how server.New mounts the scoped resource groups,
// with stub handlers in place of the real ones.
func scopeTestRouter(rec ScopeDenialRecorder) *chi.Mux {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	r := chi.NewRouter()
	r.Use(WithScopeDenialRecorder(rec))
	r.Route("/v1", func(r chi.Router) {
		r.With(RequireResourceScope(domain.ScopeWalletsRead, domain.ScopeWalletsWrite)).Route("/wallets", func(r chi.Router) {
			r.Get("/", ok)
			r.Post("/", ok)
			r.Post("/{id}/trustlines", ok)
		})
		r.With(RequireResourceScope(domain.ScopeTransfersRead, domain.ScopeTransfersWrite)).Route("/transfers", func(r chi.Router) {
			r.Get("/", ok)
			r.Post("/", ok)
			r.Post("/{id}/cancel", ok)
		})
		r.With(RequireResourceScope(domain.ScopeBatchesRead, domain.ScopeBatchesWrite)).Route("/transfers/batch", func(r chi.Router) {
			r.Post("/", ok)
			r.Get("/{batchId}", ok)
		})
		r.Route("/webhooks", func(r chi.Router) {
			r.With(RequireScope(domain.ScopeWebhooksWrite)).Post("/", ok)
			r.With(RequireScope(domain.ScopeWebhooksRead)).Get("/", ok)
		})
		r.With(RequireScope(domain.ScopeReportsRead)).Get("/usage", ok)
	})
	return r
}

type scopeCase struct {
	method, path string
	want         int
}

func runScopeCases(t *testing.T, scopes []string, cases []scopeCase) {
	t.Helper()
	router := scopeTestRouter(nil)
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(tenant.WithScopes(req.Context(), scopes))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (scopes %v)", w.Code, tc.want, scopes)
			}
		})
	}
}

func TestReadOnlyKeyCannotMutate(t *testing.T) {
	runScopeCases(t, []string{domain.ScopeWalletsRead, domain.ScopeTransfersRead}, []scopeCase{
		{http.MethodGet, "/v1/wallets/", http.StatusOK},
		{http.MethodPost, "/v1/wallets/", http.StatusForbidden},
		{http.MethodPost, "/v1/wallets/w1/trustlines", http.StatusForbidden},
		{http.MethodGet, "/v1/transfers/", http.StatusOK},
		{http.MethodPost, "/v1/transfers/", http.StatusForbidden},
		{http.MethodPost, "/v1/transfers/t1/cancel", http.StatusForbidden},
	})
}

func TestWriteOnlyKeyCannotRead(t *testing.T) {
	runScopeCases(t, []string{domain.ScopeWalletsWrite, domain.ScopeTransfersWrite}, []scopeCase{
		{http.MethodPost, "/v1/wallets/", http.StatusOK},
		{http.MethodGet, "/v1/wallets/", http.StatusForbidden},
		{http.MethodPost, "/v1/transfers/", http.StatusOK},
		{http.MethodGet, "/v1/transfers/", http.StatusForbidden},
	})
}

func TestMixedScopesAreEnforcedPerResource(t *testing.T) {
	runScopeCases(t, []string{domain.ScopeWalletsRead, domain.ScopeTransfersWrite, domain.ScopeWebhooksRead}, []scopeCase{
		{http.MethodGet, "/v1/wallets/", http.StatusOK},
		{http.MethodPost, "/v1/wallets/", http.StatusForbidden},
		{http.MethodPost, "/v1/transfers/", http.StatusOK},
		{http.MethodGet, "/v1/transfers/", http.StatusForbidden},
		{http.MethodGet, "/v1/webhooks/", http.StatusOK},
		{http.MethodPost, "/v1/webhooks/", http.StatusForbidden},
		{http.MethodGet, "/v1/usage", http.StatusForbidden},
	})
}

func TestBatchAndReportScopes(t *testing.T) {
	// A batches-only key can run batches but not single transfers; the more
	// specific /transfers/batch mount must win over /transfers.
	runScopeCases(t, []string{domain.ScopeBatchesWrite, domain.ScopeBatchesRead, domain.ScopeReportsRead}, []scopeCase{
		{http.MethodPost, "/v1/transfers/batch/", http.StatusOK},
		{http.MethodGet, "/v1/transfers/batch/b1", http.StatusOK},
		{http.MethodPost, "/v1/transfers/", http.StatusForbidden},
		{http.MethodGet, "/v1/usage", http.StatusOK},
	})
}

func TestLegacyTransferScopesKeepBatchAccess(t *testing.T) {
	runScopeCases(t, []string{domain.ScopeTransfersWrite, domain.ScopeTransfersRead}, []scopeCase{
		{http.MethodPost, "/v1/transfers/batch/", http.StatusOK},
		{http.MethodGet, "/v1/transfers/batch/b1", http.StatusOK},
	})
}

func TestUnscopedKeysAndUsersKeepFullAccess(t *testing.T) {
	all := []scopeCase{
		{http.MethodPost, "/v1/wallets/", http.StatusOK},
		{http.MethodPost, "/v1/transfers/", http.StatusOK},
		{http.MethodPost, "/v1/transfers/batch/", http.StatusOK},
		{http.MethodPost, "/v1/webhooks/", http.StatusOK},
		{http.MethodGet, "/v1/usage", http.StatusOK},
	}
	// Keys created before scopes existed were migrated with scopes = '{}'.
	t.Run("empty scopes", func(t *testing.T) { runScopeCases(t, []string{}, all) })
	t.Run("wildcard", func(t *testing.T) { runScopeCases(t, []string{domain.ScopeWildcard}, all) })

	// JWT (dashboard) requests carry no scopes in the context at all.
	router := scopeTestRouter(nil)
	for _, tc := range all {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("no-scope-context %s %s: status = %d, want 200", tc.method, tc.path, w.Code)
		}
	}
}

func TestInsufficientScopeReturnsStableErrorCode(t *testing.T) {
	router := scopeTestRouter(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/transfers/", nil)
	req = req.WithContext(tenant.WithScopes(req.Context(), []string{domain.ScopeTransfersRead}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if InsufficientScopeCode != "INSUFFICIENT_SCOPE" {
		t.Fatalf("InsufficientScopeCode changed to %q; clients depend on it", InsufficientScopeCode)
	}
	if body.Error.Code != InsufficientScopeCode {
		t.Fatalf("code = %q, want INSUFFICIENT_SCOPE", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, domain.ScopeTransfersWrite) {
		t.Fatalf("message %q should name the required scope", body.Error.Message)
	}
}

func TestScopeDenialsAreRecordedWithoutCredentials(t *testing.T) {
	var denials []ScopeDenial
	router := scopeTestRouter(func(_ *http.Request, d ScopeDenial) { denials = append(denials, d) })

	const rawKey = "sk_live_supersecretvalue"
	req := httptest.NewRequest(http.MethodPost, "/v1/wallets/", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	ctx := tenant.WithScopes(req.Context(), []string{domain.ScopeWalletsRead})
	ctx = tenant.WithAPIKeyID(ctx, "key-123")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req.WithContext(ctx))

	// Allowed requests are not recorded.
	allowed := httptest.NewRequest(http.MethodGet, "/v1/wallets/", nil)
	router.ServeHTTP(httptest.NewRecorder(), allowed.WithContext(ctx))

	if len(denials) != 1 {
		t.Fatalf("recorded %d denials, want 1", len(denials))
	}
	d := denials[0]
	if d.APIKeyID != "key-123" || d.RequiredScope != domain.ScopeWalletsWrite ||
		d.Method != http.MethodPost || d.Path != "/v1/wallets/" {
		t.Fatalf("unexpected denial: %+v", d)
	}
	if len(d.GrantedScopes) != 1 || d.GrantedScopes[0] != domain.ScopeWalletsRead {
		t.Fatalf("granted scopes = %v", d.GrantedScopes)
	}
	encoded, _ := json.Marshal(d)
	if strings.Contains(string(encoded), rawKey) || strings.Contains(string(encoded), "Bearer") {
		t.Fatalf("denial leaks credentials: %s", encoded)
	}
}

type fakeAuditLogger struct {
	action, resourceType, resourceID string
	metadata                         map[string]interface{}
}

func (f *fakeAuditLogger) Log(_ *http.Request, action, resourceType, resourceID string, metadata map[string]interface{}) {
	f.action, f.resourceType, f.resourceID, f.metadata = action, resourceType, resourceID, metadata
}

func TestAuditScopeDenialsWritesAuditEvent(t *testing.T) {
	logger := &fakeAuditLogger{}
	AuditScopeDenials(logger)(httptest.NewRequest(http.MethodPost, "/v1/wallets/", nil), ScopeDenial{
		APIKeyID:      "key-123",
		RequiredScope: domain.ScopeWalletsWrite,
		GrantedScopes: []string{domain.ScopeWalletsRead},
		Method:        http.MethodPost,
		Path:          "/v1/wallets/",
	})

	if logger.action != "api_key.scope_denied" || logger.resourceType != "api_key" || logger.resourceID != "key-123" {
		t.Fatalf("unexpected audit event: %+v", logger)
	}
	if logger.metadata["required_scope"] != domain.ScopeWalletsWrite {
		t.Fatalf("metadata = %v", logger.metadata)
	}
	for k := range logger.metadata {
		if k == "key" || k == "authorization" || k == "key_hash" {
			t.Fatalf("metadata must not contain credential field %q", k)
		}
	}
}
