package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
)

type mockRepo struct {
	events []*domain.AuditEvent
}

func (m *mockRepo) Create(ctx context.Context, event *domain.AuditEvent) error {
	m.events = append(m.events, event)
	return nil
}

func (m *mockRepo) List(ctx context.Context, tenantID string, mode domain.Mode, filter domain.AuditFilter) ([]*domain.AuditEvent, int, error) {
	var filtered []*domain.AuditEvent
	for _, e := range m.events {
		if e.TenantID == tenantID && e.Mode == mode {
			filtered = append(filtered, e)
		}
	}
	return filtered, len(filtered), nil
}

func TestRedactMetadata(t *testing.T) {
	input := map[string]interface{}{
		"api_key":      "sk_live_1234567890",
		"secret_token": "super_secret",
		"password":     "hunter2",
		"private_key":  "Sxxxxxxxx",
		"normal_field": "public_value",
		"nested": map[string]interface{}{
			"auth_token": "token123",
			"amount":     "100.00",
		},
	}

	redacted := domain.RedactMetadata(input)
	if redacted["api_key"] != "[REDACTED]" {
		t.Errorf("api_key was not redacted: %v", redacted["api_key"])
	}
	if redacted["secret_token"] != "[REDACTED]" {
		t.Errorf("secret_token was not redacted: %v", redacted["secret_token"])
	}
	if redacted["password"] != "[REDACTED]" {
		t.Errorf("password was not redacted: %v", redacted["password"])
	}
	if redacted["normal_field"] != "public_value" {
		t.Errorf("normal_field was modified: %v", redacted["normal_field"])
	}
	nested, ok := redacted["nested"].(map[string]interface{})
	if !ok || nested["auth_token"] != "[REDACTED]" {
		t.Errorf("nested auth_token was not redacted: %v", nested["auth_token"])
	}
	if nested["amount"] != "100.00" {
		t.Errorf("nested amount was modified: %v", nested["amount"])
	}
}

func TestAuditServiceExport(t *testing.T) {
	repo := &mockRepo{
		events: []*domain.AuditEvent{
			{
				ID:           "evt-1",
				TenantID:     "tenant-1",
				Mode:         domain.ModeLive,
				ActorType:    "user",
				ActorID:      "user-1",
				Action:       "transfer.created",
				ResourceType: "transfer",
				ResourceID:   "tx-1",
				Metadata:     map[string]interface{}{"amount": "10.00"},
				CreatedAt:    time.Now().UTC(),
			},
		},
	}

	svc := &service{repo: nil} // We can test Export using a mock Service implementation or handler
	_ = svc
	_ = repo
}

func TestHandlerListAndExport(t *testing.T) {
	events := []*domain.AuditEvent{
		{
			ID:           "evt-1",
			TenantID:     "t-1",
			Mode:         domain.ModeLive,
			ActorType:    "user",
			ActorID:      "u-1",
			Action:       "transfer.created",
			ResourceType: "transfer",
			ResourceID:   "tx-1",
			Metadata:     map[string]interface{}{"amount": "50.0"},
			CreatedAt:    time.Now().UTC(),
		},
	}

	mockSvc := &mockService{events: events}
	handler := NewHandler(mockSvc)

	// Test List
	req := httptest.NewRequest("GET", "/v1/audit", nil)
	req = req.WithContext(tenant.WithID(req.Context(), "t-1"))
	req = req.WithContext(tenant.WithMode(req.Context(), domain.ModeLive))
	rec := httptest.NewRecorder()
	handler.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "evt-1") {
		t.Fatalf("expected response to contain evt-1, got: %s", rec.Body.String())
	}

	// Test Export CSV
	reqCSV := httptest.NewRequest("GET", "/v1/audit/export?format=csv", nil)
	reqCSV = reqCSV.WithContext(tenant.WithID(reqCSV.Context(), "t-1"))
	reqCSV = reqCSV.WithContext(tenant.WithMode(reqCSV.Context(), domain.ModeLive))
	recCSV := httptest.NewRecorder()
	handler.Export(recCSV, reqCSV)

	if recCSV.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recCSV.Code, recCSV.Body.String())
	}
	if !strings.Contains(recCSV.Body.String(), "transfer.created") {
		t.Fatalf("expected csv output to contain action, got: %s", recCSV.Body.String())
	}
}

type mockService struct {
	events []*domain.AuditEvent
}

func (m *mockService) Record(ctx context.Context, event *domain.AuditEvent) error {
	m.events = append(m.events, event)
	return nil
}
func (m *mockService) Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{}) {
}
func (m *mockService) List(ctx context.Context, filter domain.AuditFilter) ([]*domain.AuditEvent, int, error) {
	return m.events, len(m.events), nil
}
func (m *mockService) Export(ctx context.Context, filter domain.AuditFilter, format string) ([]byte, string, error) {
	if format == "csv" {
		return []byte("id,action\nevt-1,transfer.created\n"), "text/csv", nil
	}
	return []byte(`{"events":[{"id":"evt-1"}]}`), "application/json", nil
}
