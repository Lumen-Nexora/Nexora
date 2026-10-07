package org_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/auth"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/org"
	"github.com/go-chi/chi/v5"
)

type mockService struct {
	acceptInviteResp *auth.AuthResponse
	acceptInviteErr  error
	inviteErr        error
	listMembersErr   error
	updateRoleErr    error
	removeMemberErr  error
}

func (m *mockService) InviteMember(ctx context.Context, email, role string) (*domain.OrgInvite, error) {
	return nil, m.inviteErr
}

func (m *mockService) AcceptInvite(ctx context.Context, req org.AcceptInviteRequest) (*auth.AuthResponse, error) {
	return m.acceptInviteResp, m.acceptInviteErr
}

func (m *mockService) ListMembers(ctx context.Context) ([]*domain.OrgMember, error) {
	return nil, m.listMembersErr
}

func (m *mockService) UpdateRole(ctx context.Context, memberID, role string) error {
	return m.updateRoleErr
}

func (m *mockService) RemoveMember(ctx context.Context, memberID string) error {
	return m.removeMemberErr
}

func setupOrgRouter(svc org.Service) http.Handler {
	r := chi.NewRouter()
	h := org.NewHandler(svc)
	r.Route("/org", h.Routes())
	return r
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error response is not JSON: %v", err)
	}
	if payload.Error.Code == "" || payload.Error.Message == "" || payload.Error.Status != status {
		t.Fatalf("unexpected error envelope: %+v", payload.Error)
	}
	if payload.Error.RequestID == "" || payload.Error.RequestID != rec.Header().Get("X-Request-ID") {
		t.Fatalf("request ID mismatch: body=%q header=%q", payload.Error.RequestID, rec.Header().Get("X-Request-ID"))
	}
}

func TestAcceptInvite_ErrorHandling(t *testing.T) {
	t.Run("Internal database error does not leak to client", func(t *testing.T) {
		svc := &mockService{
			acceptInviteErr: errors.New("pq: deadlock detected on relation 'org_members' tx: 88123"),
		}
		router := setupOrgRouter(svc)

		body := `{"token":"inv-token-1","name":"New User","password":"password123"}`
		req := httptest.NewRequest(http.MethodPost, "/org/invites/accept", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", rec.Code)
		}
		respBody := rec.Body.String()
		if strings.Contains(respBody, "deadlock") || strings.Contains(respBody, "org_members") || strings.Contains(respBody, "88123") {
			t.Fatalf("internal error text was leaked: %q", respBody)
		}
		assertAPIError(t, rec, http.StatusInternalServerError)
	})

	t.Run("Password too short returns 400", func(t *testing.T) {
		svc := &mockService{
			acceptInviteErr: auth.ErrPasswordTooShort,
		}
		router := setupOrgRouter(svc)

		body := `{"token":"inv-token-1","name":"New User","password":"short"}`
		req := httptest.NewRequest(http.MethodPost, "/org/invites/accept", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusBadRequest)
	})

	t.Run("Password too long returns 400", func(t *testing.T) {
		svc := &mockService{
			acceptInviteErr: auth.ErrPasswordTooLong,
		}
		router := setupOrgRouter(svc)

		body := `{"token":"inv-token-1","name":"New User","password":"` + strings.Repeat("x", 80) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/org/invites/accept", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusBadRequest)
	})

	t.Run("Invite not found returns 404", func(t *testing.T) {
		svc := &mockService{
			acceptInviteErr: domain.ErrInviteNotFound,
		}
		router := setupOrgRouter(svc)

		body := `{"token":"missing-token"}`
		req := httptest.NewRequest(http.MethodPost, "/org/invites/accept", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusNotFound)
	})
}

func TestOrganizationRoutesUseStructuredInternalErrors(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		svc    *mockService
	}{
		{"invite", http.MethodPost, "/org/members/invite", `{"email":"a@example.com","role":"admin"}`, &mockService{inviteErr: errors.New("database password leaked")}},
		{"list members", http.MethodGet, "/org/members", "", &mockService{listMembersErr: errors.New("database password leaked")}},
		{"update role", http.MethodPatch, "/org/members/user-1", `{"role":"admin"}`, &mockService{updateRoleErr: errors.New("database password leaked")}},
		{"remove member", http.MethodDelete, "/org/members/user-1", "", &mockService{removeMemberErr: errors.New("database password leaked")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			rec := httptest.NewRecorder()
			setupOrgRouter(tc.svc).ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "database password leaked") {
				t.Fatalf("internal error leaked: %q", rec.Body.String())
			}
			assertAPIError(t, rec, http.StatusInternalServerError)
		})
	}
}
