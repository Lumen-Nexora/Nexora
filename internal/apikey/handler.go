package apikey

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

func preserveRequestID(w http.ResponseWriter, r *http.Request) {
	if id := strings.TrimSpace(r.Header.Get("X-Request-ID")); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
}

type AuditLogger interface {
	Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{})
}

type Repository interface {
	Create(ctx context.Context, key *domain.APIKey) error
	GetByHash(ctx context.Context, hash string) (*domain.APIKey, error)
	GetByID(ctx context.Context, id, tenantID string, mode domain.Mode) (*domain.APIKey, error)
	ListByTenant(ctx context.Context, tenantID string, mode domain.Mode) ([]*domain.APIKey, error)
	Revoke(ctx context.Context, id string, tenantID string, mode domain.Mode) error
	UpdateLastUsed(ctx context.Context, id string) error
	UpdateExpiry(ctx context.Context, id, tenantID string, mode domain.Mode, expiresAt *time.Time, reminderDays int) error
	RecordRotationReminder(ctx context.Context, id string, at time.Time) error
	ListExpiringKeys(ctx context.Context, limit int) ([]*domain.APIKey, error)
}

type Handler struct {
	repo  Repository
	audit AuditLogger
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) WithAuditLogger(audit AuditLogger) *Handler {
	h.audit = audit
	return h
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	preserveRequestID(w, r)
	tenantID := tenant.IDFromContext(r.Context())
	mode, ok := tenant.ModeFromContext(r.Context())
	if tenantID == "" || !ok {
		api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant environment is required")
		return
	}

	var req struct {
		Label                *string    `json:"label"`
		Role                 string     `json:"role"`
		Mode                 string     `json:"mode"`
		Scopes               []string   `json:"scopes"`
		ExpiresAt            *time.Time `json:"expires_at"`
		RotationReminderDays *int       `json:"rotation_reminder_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	if req.Role == "" {
		req.Role = domain.RoleDeveloper
	}
	if req.Role != domain.RoleOwner && req.Role != domain.RoleAdmin && req.Role != domain.RoleDeveloper && req.Role != domain.RoleViewer {
		api.BadRequest(w, "invalid role")
		return
	}

	if len(req.Scopes) > 0 {
		if err := domain.ValidateScopes(req.Scopes); err != nil {
			api.BadRequest(w, err.Error())
			return
		}
	}

	// A scoped API key may only mint keys narrower than itself; otherwise a
	// keys:write key could issue a "*" (or unscoped, i.e. full-access) key.
	if callerScopes, scoped := tenant.ScopesFromContext(r.Context()); scoped && len(callerScopes) > 0 {
		if len(req.Scopes) == 0 {
			api.Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "a scoped API key cannot create a full-access key; pass explicit scopes")
			return
		}
		for _, s := range req.Scopes {
			if !domain.HasScope(callerScopes, s) {
				api.Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "API key cannot grant a scope it does not hold: "+s)
				return
			}
		}
	}

	if req.ExpiresAt != nil && req.ExpiresAt.Before(time.Now().UTC()) {
		api.BadRequest(w, "expires_at cannot be in the past")
		return
	}

	reminderDays := 7
	if req.RotationReminderDays != nil {
		if *req.RotationReminderDays < 1 || *req.RotationReminderDays > 90 {
			api.BadRequest(w, "rotation_reminder_days must be between 1 and 90")
			return
		}
		reminderDays = *req.RotationReminderDays
	}

	requestedMode := mode
	if req.Mode != "" {
		parsed, err := domain.ParseMode(req.Mode)
		if err != nil {
			api.BadRequest(w, err.Error())
			return
		}
		requestedMode = parsed
	}
	// API-key callers are environment admins, not platform key managers. A user
	// JWT may create either environment; an API key may only manage its own.
	if tenant.UserIDFromContext(r.Context()) == "" && requestedMode != mode {
		api.Error(w, http.StatusForbidden, "CROSS_MODE_KEY_OPERATION", "an API key cannot create keys for another environment")
		return
	}

	raw, prefix, err := Generate(requestedMode)
	if err != nil {
		log.Error().Err(err).Msg("generate api key")
		api.InternalError(w, err)
		return
	}

	scopes := req.Scopes
	if scopes == nil {
		scopes = []string{}
	}

	key := &domain.APIKey{
		ID:                   uuid.New().String(),
		TenantID:             tenantID,
		KeyHash:              Hash(raw),
		Prefix:               prefix,
		Mode:                 requestedMode,
		Label:                req.Label,
		Role:                 req.Role,
		Scopes:               scopes,
		ExpiresAt:            req.ExpiresAt,
		RotationReminderDays: reminderDays,
		CreatedAt:            time.Now().UTC(),
	}

	if err := h.repo.Create(r.Context(), key); err != nil {
		log.Error().Err(err).Str("tenant_id", tenantID).Str("mode", string(requestedMode)).Msg("create api key")
		api.InternalError(w, err)
		return
	}

	if h.audit != nil {
		h.audit.Log(r, "api_key.created", "api_key", key.ID, map[string]interface{}{
			"prefix":                 key.Prefix,
			"role":                   key.Role,
			"scopes":                 key.Scopes,
			"mode":                   key.Mode,
			"expires_at":             key.ExpiresAt,
			"rotation_reminder_days": key.RotationReminderDays,
		})
	}

	api.JSON(w, http.StatusCreated, map[string]interface{}{
		"id":                     key.ID,
		"key":                    raw, // raw key is returned exactly once
		"prefix":                 key.Prefix,
		"mode":                   key.Mode,
		"label":                  key.Label,
		"role":                   key.Role,
		"scopes":                 key.Scopes,
		"expires_at":             key.ExpiresAt,
		"rotation_reminder_days": key.RotationReminderDays,
		"created_at":             key.CreatedAt,
	})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	preserveRequestID(w, r)
	tenantID := tenant.IDFromContext(r.Context())
	mode, ok := tenant.ModeFromContext(r.Context())
	if tenantID == "" || !ok {
		api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant environment is required")
		return
	}

	keys, err := h.repo.ListByTenant(r.Context(), tenantID, mode)
	if err != nil {
		log.Error().Err(err).Str("tenant_id", tenantID).Str("mode", string(mode)).Msg("list api keys")
		api.InternalError(w, err)
		return
	}

	// The response shape is explicit so the stored key hash can never leak.
	res := make([]map[string]interface{}, 0, len(keys))
	now := time.Now().UTC()
	for _, k := range keys {
		isExpired := k.IsExpired(now)
		res = append(res, map[string]interface{}{
			"id":                     k.ID,
			"prefix":                 k.Prefix,
			"mode":                   k.Mode,
			"label":                  k.Label,
			"role":                   k.Role,
			"scopes":                 k.Scopes,
			"last_used_at":           k.LastUsedAt,
			"revoked_at":             k.RevokedAt,
			"expires_at":             k.ExpiresAt,
			"rotation_reminder_days": k.RotationReminderDays,
			"is_expired":             isExpired,
			"created_at":             k.CreatedAt,
		})
	}
	api.JSON(w, http.StatusOK, res)
}

func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	preserveRequestID(w, r)
	tenantID := tenant.IDFromContext(r.Context())
	mode, ok := tenant.ModeFromContext(r.Context())
	if tenantID == "" || !ok {
		api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant environment is required")
		return
	}
	id := chi.URLParam(r, "id")

	if err := h.repo.Revoke(r.Context(), id, tenantID, mode); err != nil {
		log.Error().Err(err).Str("key_id", id).Msg("revoke api key")
		api.Error(w, http.StatusNotFound, "API_KEY_NOT_FOUND", "API key not found in this environment")
		return
	}

	if h.audit != nil {
		h.audit.Log(r, "api_key.revoked", "api_key", id, map[string]interface{}{
			"id": id,
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// UpdateExpiry updates an existing API key's expiration policy.
func (h *Handler) UpdateExpiry(w http.ResponseWriter, r *http.Request) {
	preserveRequestID(w, r)
	tenantID := tenant.IDFromContext(r.Context())
	mode, ok := tenant.ModeFromContext(r.Context())
	if tenantID == "" || !ok {
		api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant environment is required")
		return
	}
	id := chi.URLParam(r, "id")

	var req struct {
		ExpiresAt            *time.Time `json:"expires_at"`
		RotationReminderDays *int       `json:"rotation_reminder_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	if req.ExpiresAt != nil && req.ExpiresAt.Before(time.Now().UTC()) {
		api.BadRequest(w, "expires_at cannot be in the past")
		return
	}

	reminderDays := 7
	if req.RotationReminderDays != nil {
		if *req.RotationReminderDays < 1 || *req.RotationReminderDays > 90 {
			api.BadRequest(w, "rotation_reminder_days must be between 1 and 90")
			return
		}
		reminderDays = *req.RotationReminderDays
	}

	if err := h.repo.UpdateExpiry(r.Context(), id, tenantID, mode, req.ExpiresAt, reminderDays); err != nil {
		log.Error().Err(err).Str("key_id", id).Msg("update api key expiry")
		api.Error(w, http.StatusNotFound, "API_KEY_NOT_FOUND", "API key not found or already revoked")
		return
	}

	if h.audit != nil {
		h.audit.Log(r, "api_key.expiry_updated", "api_key", id, map[string]interface{}{
			"id":                     id,
			"expires_at":             req.ExpiresAt,
			"rotation_reminder_days": reminderDays,
		})
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"id":                     id,
		"expires_at":             req.ExpiresAt,
		"rotation_reminder_days": reminderDays,
	})
}

// Rotate atomically revokes the specified key and provisions a fresh replacement key
// with matching role, scopes, label, mode, and renewal expiry policy.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	preserveRequestID(w, r)
	tenantID := tenant.IDFromContext(r.Context())
	mode, ok := tenant.ModeFromContext(r.Context())
	if tenantID == "" || !ok {
		api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant environment is required")
		return
	}
	id := chi.URLParam(r, "id")

	oldKey, err := h.repo.GetByID(r.Context(), id, tenantID, mode)
	if err != nil || oldKey == nil {
		api.Error(w, http.StatusNotFound, "API_KEY_NOT_FOUND", "API key not found in this environment")
		return
	}
	if oldKey.RevokedAt != nil {
		api.BadRequest(w, "cannot rotate an already revoked API key")
		return
	}

	var req struct {
		ExpiresInDays        *int `json:"expires_in_days"`
		RotationReminderDays *int `json:"rotation_reminder_days"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Determine new key expiration
	var newExpiresAt *time.Time
	if req.ExpiresInDays != nil && *req.ExpiresInDays > 0 {
		exp := time.Now().UTC().AddDate(0, 0, *req.ExpiresInDays)
		newExpiresAt = &exp
	} else if oldKey.ExpiresAt != nil {
		// Inherit lifespan if old key had an expiry
		lifespan := oldKey.ExpiresAt.Sub(oldKey.CreatedAt)
		if lifespan > 0 {
			exp := time.Now().UTC().Add(lifespan)
			newExpiresAt = &exp
		}
	}

	reminderDays := oldKey.RotationReminderDays
	if req.RotationReminderDays != nil && *req.RotationReminderDays >= 1 && *req.RotationReminderDays <= 90 {
		reminderDays = *req.RotationReminderDays
	}

	// Generate replacement credential
	raw, prefix, err := Generate(oldKey.Mode)
	if err != nil {
		log.Error().Err(err).Msg("rotate api key generate")
		api.InternalError(w, err)
		return
	}

	newKey := &domain.APIKey{
		ID:                   uuid.New().String(),
		TenantID:             tenantID,
		KeyHash:              Hash(raw),
		Prefix:               prefix,
		Mode:                 oldKey.Mode,
		Label:                oldKey.Label,
		Role:                 oldKey.Role,
		Scopes:               oldKey.Scopes,
		ExpiresAt:            newExpiresAt,
		RotationReminderDays: reminderDays,
		CreatedAt:            time.Now().UTC(),
	}

	// Persist replacement key
	if err := h.repo.Create(r.Context(), newKey); err != nil {
		log.Error().Err(err).Msg("create rotated api key")
		api.InternalError(w, err)
		return
	}

	// Revoke old key
	_ = h.repo.Revoke(r.Context(), id, tenantID, mode)

	if h.audit != nil {
		h.audit.Log(r, "api_key.rotated", "api_key", newKey.ID, map[string]interface{}{
			"previous_key_id":        id,
			"new_key_id":             newKey.ID,
			"prefix":                 newKey.Prefix,
			"expires_at":             newKey.ExpiresAt,
			"rotation_reminder_days": newKey.RotationReminderDays,
		})
	}

	api.JSON(w, http.StatusCreated, map[string]interface{}{
		"previous_key_id":        id,
		"id":                     newKey.ID,
		"key":                    raw, // returned once
		"prefix":                 newKey.Prefix,
		"mode":                   newKey.Mode,
		"label":                  newKey.Label,
		"role":                   newKey.Role,
		"scopes":                 newKey.Scopes,
		"expires_at":             newKey.ExpiresAt,
		"rotation_reminder_days": newKey.RotationReminderDays,
		"created_at":             newKey.CreatedAt,
	})
}
