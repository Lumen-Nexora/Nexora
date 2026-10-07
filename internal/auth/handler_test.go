package auth_test

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
	"github.com/go-chi/chi/v5"
)

type mockAuthService struct {
	registerResp *auth.AuthResponse
	registerErr  error
	loginResp    *auth.AuthResponse
	loginErr     error
	refreshResp  *auth.AuthResponse
	refreshErr   error
}

func (m *mockAuthService) Register(ctx context.Context, req auth.RegisterRequest) (*auth.AuthResponse, error) {
	return m.registerResp, m.registerErr
}

func (m *mockAuthService) Login(ctx context.Context, email, password string) (*auth.AuthResponse, error) {
	return m.loginResp, m.loginErr
}

func (m *mockAuthService) RefreshToken(ctx context.Context, refreshTokenStr string) (*auth.AuthResponse, error) {
	return m.refreshResp, m.refreshErr
}

func setupRouter(svc auth.Service) http.Handler {
	r := chi.NewRouter()
	h := auth.NewHandler(svc)
	r.Route("/auth", h.Routes())
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

func TestHandler_InternalErrorsNeverLeaked(t *testing.T) {
	t.Run("Login internal database error does not leak to client", func(t *testing.T) {
		svc := &mockAuthService{
			loginErr: errors.New("pq: password authentication failed for user 'postgres' host: 10.0.0.1 database 'nexora_prod'"),
		}
		router := setupRouter(svc)

		reqBody := `{"email":"ops@example.com","password":"mypassword123"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d", rec.Code)
		}

		body := rec.Body.String()
		// Must not contain database details
		if strings.Contains(body, "postgres") || strings.Contains(body, "nexora_prod") || strings.Contains(body, "10.0.0.1") {
			t.Fatalf("internal error text was leaked in response body: %q", body)
		}

		assertAPIError(t, rec, http.StatusInternalServerError)
	})

	t.Run("Register internal database error does not leak to client", func(t *testing.T) {
		svc := &mockAuthService{
			registerErr: errors.New("pq: relation 'users' does not exist at character 13 in transaction 92834"),
		}
		router := setupRouter(svc)

		reqBody := `{"name":"Alice","email":"alice@example.com","password":"validpassword123"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d", rec.Code)
		}

		body := rec.Body.String()
		if strings.Contains(body, "relation") || strings.Contains(body, "transaction") || strings.Contains(body, "92834") {
			t.Fatalf("internal error text was leaked in response body: %q", body)
		}

		assertAPIError(t, rec, http.StatusInternalServerError)
	})

	t.Run("Refresh internal error does not leak to client", func(t *testing.T) {
		svc := &mockAuthService{refreshErr: errors.New("refresh provider secret leaked")}
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewBufferString(`{"refresh_token":"token"}`))
		rec := httptest.NewRecorder()
		setupRouter(svc).ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "refresh provider secret leaked") {
			t.Fatalf("internal error text was leaked: %q", rec.Body.String())
		}
		assertAPIError(t, rec, http.StatusInternalServerError)
	})
}

func TestHandler_ExpectedStatusCodes(t *testing.T) {
	t.Run("Login invalid credentials returns 401", func(t *testing.T) {
		svc := &mockAuthService{
			loginErr: domain.ErrInvalidCredentials,
		}
		router := setupRouter(svc)

		reqBody := `{"email":"ops@example.com","password":"wrongpassword"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(reqBody))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected status 401, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusUnauthorized)
	})

	t.Run("Register user already exists returns 409", func(t *testing.T) {
		svc := &mockAuthService{
			registerErr: domain.ErrUserAlreadyExists,
		}
		router := setupRouter(svc)

		reqBody := `{"name":"Bob","email":"bob@example.com","password":"password123"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewBufferString(reqBody))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("expected status 409, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusConflict)
	})

	t.Run("Register password too short returns 400", func(t *testing.T) {
		svc := &mockAuthService{
			registerErr: auth.ErrPasswordTooShort,
		}
		router := setupRouter(svc)

		reqBody := `{"name":"Bob","email":"bob@example.com","password":"short"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewBufferString(reqBody))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusBadRequest)
	})

	t.Run("Register password too long returns 400", func(t *testing.T) {
		svc := &mockAuthService{
			registerErr: auth.ErrPasswordTooLong,
		}
		router := setupRouter(svc)

		reqBody := `{"name":"Bob","email":"bob@example.com","password":"` + strings.Repeat("x", 80) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewBufferString(reqBody))
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d", rec.Code)
		}
		assertAPIError(t, rec, http.StatusBadRequest)
	})
}
