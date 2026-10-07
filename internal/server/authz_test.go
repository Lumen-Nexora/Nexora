package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/anchor"
	"github.com/Lumen-Nexora/Nexora/internal/apikey"
	"github.com/Lumen-Nexora/Nexora/internal/auth"
	"github.com/Lumen-Nexora/Nexora/internal/batch"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fees"
	"github.com/Lumen-Nexora/Nexora/internal/fiat"
	"github.com/Lumen-Nexora/Nexora/internal/fx"
	"github.com/Lumen-Nexora/Nexora/internal/org"
	"github.com/Lumen-Nexora/Nexora/internal/reconcile"
	"github.com/Lumen-Nexora/Nexora/internal/schedule"
	"github.com/Lumen-Nexora/Nexora/internal/status"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/Lumen-Nexora/Nexora/internal/transfer"
	"github.com/Lumen-Nexora/Nexora/internal/treasury"
	"github.com/Lumen-Nexora/Nexora/internal/wallet"
	"github.com/Lumen-Nexora/Nexora/internal/webhook"
	"github.com/go-chi/chi/v5"
)

var authzJWTSecret = []byte("test-secret-authz")

// ---------------------------------------------------------------------------
// Mock membership validator
// ---------------------------------------------------------------------------

type mockMembershipValidator struct {
	// members maps "tenantID:userID" → *domain.OrgMember
	members map[string]*domain.OrgMember
}

func newMockMembershipValidator(members ...*domain.OrgMember) *mockMembershipValidator {
	m := &mockMembershipValidator{members: make(map[string]*domain.OrgMember)}
	for _, mem := range members {
		m.members[mem.TenantID+":"+mem.UserID] = mem
	}
	return m
}

func (m *mockMembershipValidator) GetMember(_ context.Context, tenantID, userID string) (*domain.OrgMember, error) {
	mem, ok := m.members[tenantID+":"+userID]
	if !ok {
		return nil, domain.ErrOrgMemberNotFound
	}
	return mem, nil
}

// nilValidator always returns not-found — useful for testing removal.
type nilValidator struct{}

func (nilValidator) GetMember(_ context.Context, _, _ string) (*domain.OrgMember, error) {
	return nil, domain.ErrOrgMemberNotFound
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newAuthzTestServerWithValidator(t *testing.T, validator MembershipValidator, statusHandlers ...*status.Handler) *Server {
	t.Helper()

	treasuryHandler := treasury.NewHandler(nil).WithMutationGate(RequireRole(domain.RoleOwner, domain.RoleAdmin))
	var statusHandler *status.Handler
	if len(statusHandlers) > 0 {
		statusHandler = statusHandlers[0]
	}

	return New(
		auth.NewHandler(nil),
		org.NewHandler(nil),
		wallet.NewHandler(nil),
		transfer.NewHandler(nil),
		fx.NewHandler(nil),
		fiat.NewHandler(nil),
		fiat.NewAnchorHandler(nil),
		anchor.NewHandler(nil),
		fees.NewHandler(nil),
		reconcile.NewHandler(nil),
		apikey.NewHandler(nil),
		nil,
		webhook.NewHandler(nil),
		batch.NewHandler(nil),
		schedule.NewHandler(nil),
		treasuryHandler,
		nil, // claimableHandler
		statusHandler,
		nil, // complianceHandler
		nil, // auditHandler
		nil, // usageHandler
		authzJWTSecret,
		"0",
		nil,
		validator,
		nil,
	)
}

func mustToken(t *testing.T, role string) string {
	t.Helper()
	tok, err := auth.GenerateToken("user-1", "tenant-1", role, "user@example.com", "access", authzJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

func mustTokenForUser(t *testing.T, userID, tenantID, role string) string {
	t.Helper()
	tok, err := auth.GenerateToken(userID, tenantID, role, "user@example.com", "access", authzJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

func doRequestWithToken(t *testing.T, srv *Server, method, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	return rec.Code
}

func doRequest(t *testing.T, srv *Server, method, path, role string) int {
	t.Helper()
	return doRequestWithToken(t, srv, method, path, mustToken(t, role))
}

func TestScheduleRoutesRequireTransferReadAndWriteScopes(t *testing.T) {
	router := chi.NewRouter()
	h := schedule.NewHandler(nil)
	router.Route("/v1/schedules", h.Routes(
		RequireScope(domain.ScopeTransfersRead),
		RequireScope(domain.ScopeTransfersWrite),
	))

	for _, tc := range []struct {
		name   string
		method string
		path   string
		scopes []string
	}{
		{name: "read requires transfers:read", method: http.MethodGet, path: "/v1/schedules/", scopes: []string{domain.ScopeTransfersWrite}},
		{name: "create requires transfers:write", method: http.MethodPost, path: "/v1/schedules/", scopes: []string{domain.ScopeTransfersRead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(tenant.WithScopes(req.Context(), tc.scopes))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", w.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Existing tests (updated to pass validator)
// ---------------------------------------------------------------------------

func TestAdminRoutesRequireOwnerOrAdmin(t *testing.T) {
	routes := []struct {
		method string
		path   string
		// platformOperator routes are additionally gated by
		// RequirePlatformOperator, so an org Owner/Admin without the
		// platform-operator claim is still refused (403).
		platformOperator bool
	}{
		{http.MethodGet, "/v1/admin/fees/collected", false},
		{http.MethodGet, "/v1/admin/anchors", false},
		{http.MethodPost, "/v1/admin/anchors", false},
		{http.MethodGet, "/v1/admin/reconciliation/summary", false},
		{http.MethodPost, "/v1/admin/reconciliation/run", false},
		{http.MethodGet, "/v1/admin/treasury/balances", true},
		{http.MethodPost, "/v1/admin/treasury/sweep", true},
		{http.MethodPut, "/v1/admin/treasury/config", true},
	}

	for _, rt := range routes {
		for _, role := range []string{domain.RoleViewer, domain.RoleDeveloper} {
			t.Run(rt.method+" "+rt.path+"/"+role, func(t *testing.T) {
				// Mint a token with the JWT role, but the validator maps
				// user-1/tenant-1 to "owner", so the middleware will override
				// the role. We need per-role validators for this test.
				roleValidator := newMockMembershipValidator(
					&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: role},
				)
				srv := newAuthzTestServerWithValidator(t, roleValidator)
				code := doRequest(t, srv, rt.method, rt.path, role)
				if code != http.StatusForbidden {
					t.Fatalf("role %q on %s %s: expected 403, got %d", role, rt.method, rt.path, code)
				}
			})
		}

		for _, role := range []string{domain.RoleAdmin, domain.RoleOwner} {
			t.Run(rt.method+" "+rt.path+"/"+role, func(t *testing.T) {
				roleValidator := newMockMembershipValidator(
					&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: role},
				)
				srv := newAuthzTestServerWithValidator(t, roleValidator)
				code := doRequest(t, srv, rt.method, rt.path, role)
				if rt.platformOperator {
					if code != http.StatusForbidden {
						t.Fatalf("role %q on %s %s: expected 403 without the platform-operator claim, got %d", role, rt.method, rt.path, code)
					}
					return
				}
				if code == http.StatusForbidden || code == http.StatusUnauthorized {
					t.Fatalf("role %q on %s %s: expected to pass authorization, got %d", role, rt.method, rt.path, code)
				}
			})
		}
	}
}

func TestOperationalRoutesAllowDeveloper(t *testing.T) {
	// Viewer gets 403 on RequireNotViewer
	viewerValidator := newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleViewer},
	)
	srv := newAuthzTestServerWithValidator(t, viewerValidator)
	code := doRequest(t, srv, http.MethodGet, "/v1/fx/rates", domain.RoleViewer)
	if code != http.StatusForbidden {
		t.Fatalf("viewer on mutating-capable group: expected 403, got %d", code)
	}

	// Developer, admin, owner pass
	for _, role := range []string{domain.RoleDeveloper, domain.RoleAdmin, domain.RoleOwner} {
		roleValidator := newMockMembershipValidator(
			&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: role},
		)
		srv := newAuthzTestServerWithValidator(t, roleValidator)
		code := doRequest(t, srv, http.MethodGet, "/v1/fx/rates", role)
		if code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Fatalf("role %q on operational route: expected to pass authorization, got %d", role, code)
		}
	}
}

// ---------------------------------------------------------------------------
// New tests: membership revalidation
// ---------------------------------------------------------------------------

// TestRemovedMemberReturns403 verifies that a user whose membership has been
// deleted gets a 403 even with a valid JWT.
func TestRemovedMemberReturns403(t *testing.T) {
	srv := newAuthzTestServerWithValidator(t, &nilValidator{})

	code := doRequest(t, srv, http.MethodGet, "/v1/fx/rates", domain.RoleAdmin)
	if code != http.StatusForbidden {
		t.Fatalf("removed member: expected 403, got %d", code)
	}
}

func TestV1MiddlewareErrorIncludesRequestID(t *testing.T) {
	srv := newAuthzTestServerWithValidator(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/keys/", nil)
	req.Header.Set("X-Request-ID", "req-middleware-test")
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode middleware error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized || body.Error.Status != rec.Code {
		t.Fatalf("status mismatch: HTTP %d, body %d", rec.Code, body.Error.Status)
	}
	if body.Error.Code != "UNAUTHORIZED" || body.Error.RequestID != "req-middleware-test" {
		t.Fatalf("unexpected middleware error: %+v", body.Error)
	}
	if rec.Header().Get("X-Request-ID") != body.Error.RequestID {
		t.Fatalf("request ID header mismatch: %q", rec.Header().Get("X-Request-ID"))
	}
}

func TestV1RoleMiddlewareErrorIncludesRequestID(t *testing.T) {
	validator := newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleDeveloper},
	)
	srv := newAuthzTestServerWithValidator(t, validator)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/fees/collected", nil)
	req.Header.Set("Authorization", "Bearer "+mustToken(t, domain.RoleDeveloper))
	req.Header.Set("X-Request-ID", "req-role-test")
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode role middleware error: %v", err)
	}
	if rec.Code != http.StatusForbidden || body.Error.Status != rec.Code || body.Error.Code != "FORBIDDEN" {
		t.Fatalf("unexpected role middleware error: HTTP %d, body %+v", rec.Code, body.Error)
	}
	if body.Error.RequestID != "req-role-test" || rec.Header().Get("X-Request-ID") != body.Error.RequestID {
		t.Fatalf("request ID mismatch: body=%q header=%q", body.Error.RequestID, rec.Header().Get("X-Request-ID"))
	}
}

func TestV1RouterErrorsUseStructuredEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"not found", http.MethodGet, "/v1/not-a-route", http.StatusNotFound, "NOT_FOUND"},
		{"method not allowed", http.MethodPut, "/v1/auth/login", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAuthzTestServerWithValidator(t, nil)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("X-Request-ID", "req-router-test")
			rec := httptest.NewRecorder()
			srv.router.ServeHTTP(rec, req)

			var body struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					Status    int    `json:"status"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode router error: %v", err)
			}
			if rec.Code != tc.wantStatus || body.Error.Status != tc.wantStatus || body.Error.Code != tc.wantCode {
				t.Fatalf("unexpected router error: HTTP %d, body %+v", rec.Code, body.Error)
			}
			if body.Error.RequestID != "req-router-test" || rec.Header().Get("X-Request-ID") != body.Error.RequestID {
				t.Fatalf("request ID mismatch: body=%q header=%q", body.Error.RequestID, rec.Header().Get("X-Request-ID"))
			}
		})
	}
}

// TestDemotedMemberUsesCurrentRole verifies that a user who was demoted from
// admin to developer via DB gets the downgraded role on the request, and is
// then rejected by RequireRole for admin-only routes.
func TestDemotedMemberUsesCurrentRole(t *testing.T) {
	// Token says "admin", but DB says "developer"
	demotedValidator := newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleDeveloper},
	)
	srv := newAuthzTestServerWithValidator(t, demotedValidator)

	code := doRequest(t, srv, http.MethodGet, "/v1/admin/fees/collected", domain.RoleAdmin)
	if code != http.StatusForbidden {
		t.Fatalf("demoted admin: expected 403, got %d", code)
	}
}

// TestPromotedMemberUsesCurrentRole verifies that a user promoted in the DB
// (token says "developer", DB says "admin") gains elevated access immediately.
func TestPromotedMemberUsesCurrentRole(t *testing.T) {
	// Token says "developer", but DB says "admin"
	promotedValidator := newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleAdmin},
	)
	srv := newAuthzTestServerWithValidator(t, promotedValidator)

	code := doRequest(t, srv, http.MethodGet, "/v1/admin/fees/collected", domain.RoleDeveloper)
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Fatalf("promoted developer: expected to pass, got %d", code)
	}
}

// TestRevokedMembershipReturns403 verifies the tenant context is not supplied
// by stale claims when membership is revoked.
func TestRevokedMembershipReturns403(t *testing.T) {
	srv := newAuthzTestServerWithValidator(t, &nilValidator{})

	// Try an admin-only route — should be 403, not 404 or 200.
	code := doRequest(t, srv, http.MethodGet, "/v1/admin/anchors", domain.RoleOwner)
	if code != http.StatusForbidden {
		t.Fatalf("revoked membership on admin route: expected 403, got %d", code)
	}
}

// TestCrossTenantMembershipRejected verifies that a valid JWT for tenant-A
// is rejected when the validator has no membership for that user in tenant-A.
func TestCrossTenantMembershipRejected(t *testing.T) {
	// Validator only knows tenant-1, but token claims tenant-2
	validator := newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleAdmin},
	)
	srv := newAuthzTestServerWithValidator(t, validator)

	tok := mustTokenForUser(t, "user-1", "tenant-2", domain.RoleAdmin)
	code := doRequestWithToken(t, srv, http.MethodGet, "/v1/fx/rates", tok)
	if code != http.StatusForbidden {
		t.Fatalf("cross-tenant membership: expected 403, got %d", code)
	}
}

// TestRoleMismatchUsesDBRole verifies that even if the JWT claims owner,
// the middleware enforces the DB role. The request reaches a route that only
// allows owner/admin — if the DB says "viewer", it should be 403.
func TestRoleMismatchUsesDBRole(t *testing.T) {
	validator := newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleViewer},
	)
	srv := newAuthzTestServerWithValidator(t, validator)

	code := doRequest(t, srv, http.MethodGet, "/v1/admin/fees/collected", domain.RoleOwner)
	if code != http.StatusForbidden {
		t.Fatalf("role mismatch (JWT=owner, DB=viewer): expected 403, got %d", code)
	}
}

func TestWebhookSigningSecretRotationRequiresOwnerOrAdmin(t *testing.T) {
	for _, role := range []string{domain.RoleViewer, domain.RoleDeveloper} {
		validator := newMockMembershipValidator(
			&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: role},
		)
		srv := newAuthzTestServerWithValidator(t, validator)
		code := doRequest(t, srv, http.MethodPost, "/v1/webhooks/secret/rotate", role)
		if code != http.StatusForbidden {
			t.Fatalf("role %q on webhook secret rotation: expected 403, got %d", role, code)
		}
	}
}
