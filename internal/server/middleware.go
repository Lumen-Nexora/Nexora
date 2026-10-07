package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/apikey"
	"github.com/Lumen-Nexora/Nexora/internal/auth"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/postgres"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(
			log.Logger.With().
				Str("request_id", id).
				Str("operation", "http_request").
				Logger().
				WithContext(r.Context()),
		))
	})
}

func logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		zerolog.Ctx(r.Context()).Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", ww.Status()).
			Dur("duration", time.Since(start)).
			Msg("request")
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				zerolog.Ctx(r.Context()).Error().Interface("panic", rv).Msg("panic recovered")
				api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[strings.TrimSpace(o)] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if _, ok := originSet[origin]; ok || (len(originSet) == 1 && originSet["*"] == struct{}{}) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			} else {
				// Handle prefix matching for things like localhost:*
				for allowed := range originSet {
					if strings.HasSuffix(allowed, "*") {
						prefix := strings.TrimSuffix(allowed, "*")
						if strings.HasPrefix(origin, prefix) {
							w.Header().Set("Access-Control-Allow-Origin", origin)
							break
						}
					}
				}
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-Request-ID, X-Nexora-Mode")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// MembershipValidator revalidates a user's current membership and role
// against the database on every authenticated request.
type MembershipValidator interface {
	GetMember(ctx context.Context, tenantID, userID string) (*domain.OrgMember, error)
}

// AuthMiddleware validates the JWT or API key and, for JWT auth, revalidates
// the user's membership and role against the database so that demotions,
// removals, and role changes take effect immediately rather than at token
// expiry. It also pins the request to exactly one environment: an API key
// carries its environment in the credential itself, while a user JWT selects
// one explicitly.
func AuthMiddleware(repo *postgres.APIKeyRepo, jwtSecret []byte, validator MembershipValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || len(authHeader) <= 7 || authHeader[:7] != "Bearer " {
				api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid authorization header")
				return
			}

			rawToken := authHeader[7:]

			// Check if token is JWT (contains 2 dots)
			if strings.Count(rawToken, ".") == 2 {
				claims, err := auth.ParseToken(rawToken, jwtSecret)
				if err == nil && claims.TokenType == "access" {
					// Revalidate membership against the database so stale tokens
					// cannot be used after removal or demotion.
					if validator != nil {
						member, mErr := validator.GetMember(r.Context(), claims.TenantID, claims.Sub)
						if mErr != nil && !errors.Is(mErr, domain.ErrOrgMemberNotFound) {
							api.InternalError(w, mErr)
							return
						}
						if mErr != nil || member == nil {
							api.Error(w, http.StatusForbidden, "FORBIDDEN", "membership not found or revoked")
							return
						}
						// Use the current role from the database, not the stale JWT claim.
						claims.Role = member.Role
					}
					mode := domain.ModeLive
					if requested := r.Header.Get("X-Nexora-Mode"); requested != "" {
						parsed, parseErr := domain.ParseMode(requested)
						if parseErr != nil {
							api.Error(w, http.StatusBadRequest, "INVALID_ENVIRONMENT_MODE", parseErr.Error())
							return
						}
						mode = parsed
					}
					ctx := tenant.WithID(r.Context(), claims.TenantID)
					ctx = tenant.WithMode(ctx, mode)
					ctx = tenant.WithUser(ctx, claims.Sub, claims.Role)
					requestLogger := zerolog.Ctx(ctx).With().
						Str("tenant_id", claims.TenantID).
						Logger()
					ctx = requestLogger.WithContext(ctx)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// Fallback to API Key auth
			hash := apikey.Hash(rawToken)
			key, err := repo.GetByHash(r.Context(), hash)
			if err != nil {
				api.InternalError(w, err)
				return
			}
			if key == nil {
				api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid api key or authentication token")
				return
			}
			if key.RevokedAt != nil {
				api.Error(w, http.StatusUnauthorized, "API_KEY_REVOKED", "revoked api key")
				return
			}
			if key.IsExpired(time.Now().UTC()) {
				api.Error(w, http.StatusUnauthorized, "API_KEY_EXPIRED", "expired api key")
				return
			}
			// The environment is a property of the credential, not of the
			// request, so a key minted for one environment can never reach the
			// other even if the raw key collides with a persisted record.
			rawMode, modeErr := apikey.ModeFromRaw(rawToken)
			if modeErr != nil || rawMode != key.Mode {
				api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "api key mode does not match persisted authorization")
				return
			}

			_ = repo.UpdateLastUsed(r.Context(), key.ID)

			ctx := tenant.WithID(r.Context(), key.TenantID)
			ctx = tenant.WithMode(ctx, key.Mode)
			ctx = tenant.WithUser(ctx, "", key.Role)
			ctx = tenant.WithScopes(ctx, key.Scopes)
			ctx = tenant.WithAPIKeyID(ctx, key.ID)
			requestLogger := zerolog.Ctx(ctx).With().
				Str("tenant_id", key.TenantID).
				Str("mode", string(key.Mode)).
				Logger()
			ctx = requestLogger.WithContext(ctx)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ScopeDenial describes an API-key request refused for missing a scope. It
// deliberately carries no credential material: only the key's ID, what was
// required, and what the key was granted.
type ScopeDenial struct {
	APIKeyID      string
	RequiredScope string
	GrantedScopes []string
	Method        string
	Path          string
}

// ScopeDenialRecorder persists scope denials, typically to the audit log.
type ScopeDenialRecorder func(r *http.Request, denial ScopeDenial)

type scopeDenialRecorderKey struct{}

// WithScopeDenialRecorder makes rec available to every RequireScope check
// further down the chain, including ones mounted inside handler packages.
func WithScopeDenialRecorder(rec ScopeDenialRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec != nil {
				r = r.WithContext(context.WithValue(r.Context(), scopeDenialRecorderKey{}, rec))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AuditScopeDenials records each denial as an "api_key.scope_denied" audit
// event against the key.
func AuditScopeDenials(logger interface {
	Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{})
}) ScopeDenialRecorder {
	return func(r *http.Request, d ScopeDenial) {
		logger.Log(r, "api_key.scope_denied", "api_key", d.APIKeyID, map[string]interface{}{
			"required_scope": d.RequiredScope,
			"granted_scopes": d.GrantedScopes,
			"method":         d.Method,
			"path":           d.Path,
		})
	}
}

// InsufficientScopeCode is the stable error code returned with 403 when an
// API key lacks the scope a route requires.
const InsufficientScopeCode = "INSUFFICIENT_SCOPE"

// RequireScope refuses API-key requests whose key does not grant
// requiredScope. JWT (dashboard user) requests carry no scopes and are
// governed by role checks instead.
func RequireScope(requiredScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !checkScope(w, r, requiredScope) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireResourceScope applies read/write scopes by HTTP method: safe methods
// (GET, HEAD, OPTIONS) need readScope, anything that can change state needs
// writeScope. Use it on a whole route group so new mutating endpoints can't
// slip through behind a read scope.
func RequireResourceScope(readScope, writeScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			required := writeScope
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				required = readScope
			}
			if !checkScope(w, r, required) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func checkScope(w http.ResponseWriter, r *http.Request, requiredScope string) bool {
	scopes, hasScopes := tenant.ScopesFromContext(r.Context())
	if !hasScopes || domain.HasScope(scopes, requiredScope) {
		return true
	}

	if rec, ok := r.Context().Value(scopeDenialRecorderKey{}).(ScopeDenialRecorder); ok {
		rec(r, ScopeDenial{
			APIKeyID:      tenant.APIKeyIDFromContext(r.Context()),
			RequiredScope: requiredScope,
			GrantedScopes: append([]string(nil), scopes...),
			Method:        r.Method,
			Path:          r.URL.Path,
		})
	}
	api.Error(w, http.StatusForbidden, InsufficientScopeCode, "API key does not have the required scope: "+requiredScope)
	return false
}

func RequireRole(allowedRoles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := tenant.RoleFromContext(r.Context())
			if role == "" {
				api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
				return
			}

			for _, allowed := range allowedRoles {
				if role == allowed {
					next.ServeHTTP(w, r)
					return
				}
			}

			api.Error(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		})
	}
}

// RequireTestMode protects sandbox-only routes. API-key requests always carry
// the persisted key mode, so a client-supplied header cannot bypass it.
func RequireTestMode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, ok := tenant.ModeFromContext(r.Context())
		if !ok {
			api.Error(w, http.StatusUnauthorized, "ENVIRONMENT_REQUIRED", "an authenticated environment is required")
			return
		}
		if mode != domain.ModeTest {
			api.Error(w, http.StatusForbidden, "TEST_MODE_REQUIRED", "this endpoint requires an sk_test_ API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RequireNotViewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := tenant.RoleFromContext(r.Context())
		if role == domain.RoleViewer {
			api.Error(w, http.StatusForbidden, "FORBIDDEN", "viewer role is read-only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RequirePlatformOperator() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := tenant.IDFromContext(r.Context())
			if tenantID != "platform" && tenantID != "system" && tenantID != "operator" {
				api.Error(w, http.StatusForbidden, "FORBIDDEN", "unauthorized: platform operator access required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
