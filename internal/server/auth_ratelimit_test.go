package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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
	"github.com/Lumen-Nexora/Nexora/internal/server"
	"github.com/Lumen-Nexora/Nexora/internal/transfer"
	"github.com/Lumen-Nexora/Nexora/internal/treasury"
	"github.com/Lumen-Nexora/Nexora/internal/wallet"
	"github.com/Lumen-Nexora/Nexora/internal/webhook"
	"github.com/go-chi/chi/v5"
)

type dummyAuthService struct{}

func (d *dummyAuthService) Register(ctx context.Context, req auth.RegisterRequest) (*auth.AuthResponse, error) {
	return &auth.AuthResponse{User: &domain.User{ID: "u1", Email: req.Email}}, nil
}

func (d *dummyAuthService) Login(ctx context.Context, email, password string) (*auth.AuthResponse, error) {
	return &auth.AuthResponse{User: &domain.User{ID: "u1", Email: email}}, nil
}

func (d *dummyAuthService) RefreshToken(ctx context.Context, token string) (*auth.AuthResponse, error) {
	return &auth.AuthResponse{}, nil
}

type dummyOrgService struct{}

func (d *dummyOrgService) InviteMember(ctx context.Context, email, role string) (*domain.OrgInvite, error) {
	return &domain.OrgInvite{}, nil
}

func (d *dummyOrgService) AcceptInvite(ctx context.Context, req org.AcceptInviteRequest) (*auth.AuthResponse, error) {
	return &auth.AuthResponse{}, nil
}

func (d *dummyOrgService) ListMembers(ctx context.Context) ([]*domain.OrgMember, error) {
	return nil, nil
}

func (d *dummyOrgService) UpdateRole(ctx context.Context, memberID, role string) error {
	return nil
}

func (d *dummyOrgService) RemoveMember(ctx context.Context, memberID string) error {
	return nil
}

func setupTestServerWithRateLimit(cfg server.AuthRateLimitConfig) *chi.Mux {
	authH := auth.NewHandler(&dummyAuthService{})
	orgH := org.NewHandler(&dummyOrgService{})

	treasuryH := treasury.NewHandler(nil)

	srv := server.New(
		authH,
		orgH,
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
		treasuryH,
		nil, // claimableHandler
		nil, // statusHandler
		nil, // complianceHandler
		nil, // auditHandler
		nil, // usageHandler
		[]byte("test-jwt-secret-key-32-bytes-long!"),
		"0",
		nil,
		nil,
		nil,
		cfg,
	)
	return srv.Router()
}

func TestAuthRateLimiter_PerIPLimiting(t *testing.T) {
	// Configure low burst for testing: IP burst 3, Account burst 10
	cfg := server.AuthRateLimitConfig{
		IPRPS:        1,
		IPBurst:      3,
		AccountRPS:   10,
		AccountBurst: 10,
	}
	router := setupTestServerWithRateLimit(cfg)

	// Test on /v1/auth/login
	for i := 1; i <= 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"email":"user`+strconv.Itoa(i)+`@example.com","password":"password123"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d expected status 200, got %d", i, rec.Code)
		}
	}

	// 4th request from same IP exceeds IP burst
	req4 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"email":"user4@example.com","password":"password123"}`))
	req4.Header.Set("Content-Type", "application/json")
	req4.RemoteAddr = "192.0.2.1:1234"
	rec4 := httptest.NewRecorder()

	router.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %d: %s", rec4.Code, rec4.Body.String())
	}

	retryAfter := rec4.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header on 429 response, got none")
	}
	retrySec, err := strconv.Atoi(retryAfter)
	if err != nil || retrySec <= 0 {
		t.Fatalf("invalid Retry-After header: %q", retryAfter)
	}

	if !strings.Contains(rec4.Body.String(), "RATE_LIMITED") {
		t.Fatalf("expected RATE_LIMITED error code in body: %s", rec4.Body.String())
	}

	// A different IP should still succeed
	reqOtherIP := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"email":"user5@example.com","password":"password123"}`))
	reqOtherIP.Header.Set("Content-Type", "application/json")
	reqOtherIP.RemoteAddr = "192.0.2.2:1234"
	recOtherIP := httptest.NewRecorder()

	router.ServeHTTP(recOtherIP, reqOtherIP)
	if recOtherIP.Code != http.StatusOK {
		t.Fatalf("expected status 200 from different IP, got %d", recOtherIP.Code)
	}
}

func TestAuthRateLimiter_PerAccountLimiting(t *testing.T) {
	// Configure high IP burst, low account burst: IP burst 10, Account burst 2
	cfg := server.AuthRateLimitConfig{
		IPRPS:        10,
		IPBurst:      10,
		AccountRPS:   1,
		AccountBurst: 2,
	}
	router := setupTestServerWithRateLimit(cfg)

	targetEmail := "victim@example.com"

	// 2 requests for victim@example.com from DIFFERENT IPs
	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"email":"`+targetEmail+`","password":"password123"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2." + strconv.Itoa(i) + ":1234"
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d expected status 200, got %d", i, rec.Code)
		}
	}

	// 3rd request for same account from yet another IP should trigger per-account rate limit
	req3 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"email":"`+targetEmail+`","password":"password123"}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.RemoteAddr = "192.0.2.99:1234"
	rec3 := httptest.NewRecorder()

	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on targeted account, got %d: %s", rec3.Code, rec3.Body.String())
	}

	retryAfter := rec3.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header on 429 response, got none")
	}

	// Different account should succeed
	reqOtherAcct := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"email":"other@example.com","password":"password123"}`))
	reqOtherAcct.Header.Set("Content-Type", "application/json")
	reqOtherAcct.RemoteAddr = "192.0.2.100:1234"
	recOtherAcct := httptest.NewRecorder()

	router.ServeHTTP(recOtherAcct, reqOtherAcct)
	if recOtherAcct.Code != http.StatusOK {
		t.Fatalf("expected status 200 for different account, got %d", recOtherAcct.Code)
	}
}

func TestAuthRateLimiter_RegisterAndInviteAcceptEndpoints(t *testing.T) {
	cfg := server.AuthRateLimitConfig{
		IPRPS:        1,
		IPBurst:      2,
		AccountRPS:   1,
		AccountBurst: 2,
	}
	router := setupTestServerWithRateLimit(cfg)

	t.Run("POST /v1/auth/register is rate limited", func(t *testing.T) {
		for i := 1; i <= 2; i++ {
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", bytes.NewBufferString(`{"name":"User","email":"reg`+strconv.Itoa(i)+`@example.com","password":"password123"}`))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = "10.0.0.1:1234"
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("request %d expected status 201, got %d", i, rec.Code)
			}
		}

		req3 := httptest.NewRequest(http.MethodPost, "/v1/auth/register", bytes.NewBufferString(`{"name":"User","email":"reg3@example.com","password":"password123"}`))
		req3.Header.Set("Content-Type", "application/json")
		req3.RemoteAddr = "10.0.0.1:1234"
		rec3 := httptest.NewRecorder()
		router.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 for register, got %d: %s", rec3.Code, rec3.Body.String())
		}
		if rec3.Header().Get("Retry-After") == "" {
			t.Fatal("expected Retry-After header for register 429")
		}
	})

	t.Run("POST /v1/org/invites/accept is rate limited", func(t *testing.T) {
		for i := 1; i <= 2; i++ {
			req := httptest.NewRequest(http.MethodPost, "/v1/org/invites/accept", bytes.NewBufferString(`{"token":"token-`+strconv.Itoa(i)+`"}`))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = "10.0.0.2:1234"
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("request %d expected status 200, got %d", i, rec.Code)
			}
		}

		req3 := httptest.NewRequest(http.MethodPost, "/v1/org/invites/accept", bytes.NewBufferString(`{"token":"token-3"}`))
		req3.Header.Set("Content-Type", "application/json")
		req3.RemoteAddr = "10.0.0.2:1234"
		rec3 := httptest.NewRecorder()
		router.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 for invites/accept, got %d: %s", rec3.Code, rec3.Body.String())
		}
		if rec3.Header().Get("Retry-After") == "" {
			t.Fatal("expected Retry-After header for invites/accept 429")
		}
	})
}
