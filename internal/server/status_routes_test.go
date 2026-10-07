package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/status"
	"github.com/google/uuid"
)

type statusMemoryRepository struct {
	mu    sync.Mutex
	items map[string]domain.Incident
}

func newStatusMemoryRepository() *statusMemoryRepository {
	return &statusMemoryRepository{items: make(map[string]domain.Incident)}
}

func (r *statusMemoryRepository) Create(_ context.Context, inc *domain.Incident) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inc.ID == "" {
		inc.ID = uuid.NewString()
	}
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = time.Now().UTC()
	}
	r.items[inc.ID] = *inc
	return nil
}

func (r *statusMemoryRepository) GetByID(_ context.Context, id string) (*domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inc, ok := r.items[id]
	if !ok {
		return nil, domain.ErrIncidentNotFound
	}
	return &inc, nil
}

func (r *statusMemoryRepository) List(_ context.Context, limit int) ([]domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]domain.Incident, 0, len(r.items))
	for _, inc := range r.items {
		result = append(result, inc)
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (r *statusMemoryRepository) ListActive(_ context.Context) ([]domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]domain.Incident, 0)
	for _, inc := range r.items {
		if inc.Status != string(domain.StatusResolved) {
			result = append(result, inc)
		}
	}
	return result, nil
}

func (r *statusMemoryRepository) Update(_ context.Context, inc *domain.Incident) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[inc.ID]; !ok {
		return domain.ErrIncidentNotFound
	}
	r.items[inc.ID] = *inc
	return nil
}

func statusTestServer(t *testing.T, repo status.Repository) *Server {
	t.Helper()
	return newAuthzTestServerWithValidator(t, newMockMembershipValidator(
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-1", Role: domain.RoleOwner},
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-2", Role: domain.RoleViewer},
		&domain.OrgMember{TenantID: "tenant-1", UserID: "user-3", Role: domain.RoleDeveloper},
	), status.NewHandler(status.NewService(repo)))
}

func requestJSON(t *testing.T, srv *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	return rec
}

func TestStatusRoutesArePublic(t *testing.T) {
	srv := statusTestServer(t, newStatusMemoryRepository())
	for _, path := range []string{"/status", "/status/incidents"} {
		rec := requestJSON(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestStatusIncidentMutationsRequireOwnerOrAdmin(t *testing.T) {
	repo := newStatusMemoryRepository()
	srv := statusTestServer(t, repo)
	viewer := mustTokenForUser(t, "user-2", "tenant-1", domain.RoleViewer)
	developer := mustTokenForUser(t, "user-3", "tenant-1", domain.RoleDeveloper)
	createBody := `{"title":"API issue","description":"Investigating","severity":"major"}`
	for _, role := range []string{viewer, developer} {
		rec := requestJSON(t, srv, http.MethodPost, "/v1/admin/incidents", role, createBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role %s create status = %d, want 403", role, rec.Code)
		}
	}
	if len(repo.items) != 0 {
		t.Fatalf("denied mutations persisted %d incidents", len(repo.items))
	}
}

func TestStatusIncidentPersistenceAndLifecycle(t *testing.T) {
	repo := newStatusMemoryRepository()
	srv := statusTestServer(t, repo)
	owner := mustToken(t, domain.RoleOwner)
	create := requestJSON(t, srv, http.MethodPost, "/v1/admin/incidents", owner, `{"title":"API latency","description":"Investigating","severity":"major"}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d, body=%s", create.Code, create.Body.String())
	}
	repo.mu.Lock()
	var id string
	for itemID := range repo.items {
		id = itemID
	}
	repo.mu.Unlock()
	if id == "" {
		t.Fatal("create did not persist an incident")
	}

	for _, next := range []string{"identified", "monitoring", "resolved"} {
		rec := requestJSON(t, srv, http.MethodPatch, "/v1/admin/incidents/"+id, owner, `{"status":"`+next+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("transition to %s = %d, body=%s", next, rec.Code, rec.Body.String())
		}
	}
	public := requestJSON(t, srv, http.MethodGet, "/status", "", "")
	if public.Code != http.StatusOK || !strings.Contains(public.Body.String(), `"status":"operational"`) {
		t.Fatalf("resolved status = %d, body=%s", public.Code, public.Body.String())
	}
}
