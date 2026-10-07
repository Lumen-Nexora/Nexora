package idempotency

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
)

// MaxInspectionResponseBodyBytes is the maximum stored response body size returned
// by the inspection endpoint (64 KB). Bodies exceeding this limit are truncated
// with body_truncated set to true.
const MaxInspectionResponseBodyBytes = 64 * 1024

// InspectionResponse represents the inspection result for an idempotency key.
type InspectionResponse struct {
	Key               string          `json:"key"`
	Status            string          `json:"status"` // "processing", "completed", "failed", "unknown"
	CreatedAt         *time.Time      `json:"created_at,omitempty"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	RequestHash       string          `json:"request_hash,omitempty"`
	RetryAfterSeconds *int            `json:"retry_after_seconds,omitempty"`
	RetryHint         string          `json:"retry_hint,omitempty"`
	Response          *StoredResponse `json:"response,omitempty"`
}

// StoredResponse holds the faithfully reproduced status, headers, and body
// from a completed or failed idempotent request.
type StoredResponse struct {
	Status        int             `json:"status"`
	Headers       http.Header     `json:"headers,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
	BodyText      string          `json:"body_text,omitempty"`
	BodyTruncated bool            `json:"body_truncated"`
	BodySizeBytes int             `json:"body_size_bytes"`
}

// Handler provides read-only idempotency key inspection.
type Handler struct {
	repo Repository
	now  func() time.Time
}

// NewHandler constructs an idempotency inspection Handler.
func NewHandler(repo Repository) *Handler {
	return &Handler{
		repo: repo,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// WithClock overrides the internal clock provider (used in unit tests).
func (h *Handler) WithClock(now func() time.Time) *Handler {
	h.now = now
	return h
}

// Routes returns a route configuration function for chi.Router.
func (h *Handler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/{key}", h.Inspect)
	}
}

// Inspect handles GET /v1/idempotency/{key}.
// It is read-only, tenant-scoped, and does not acquire leases or mutate records.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	rawKey := chi.URLParam(r, "key")
	if rawKey == "" {
		api.BadRequest(w, "idempotency key is required")
		return
	}

	key := DeterministicKey(rawKey)
	orgID := tenant.IDFromContext(r.Context())
	mode := tenant.ModeOrDefault(r.Context(), domain.ModeLive)

	lookupRepo, ok := h.repo.(LookupRepository)
	if !ok {
		api.InternalError(w, fmt.Errorf("idempotency repository does not support inspection"))
		return
	}
	result, err := lookupRepo.Lookup(r.Context(), orgID, mode, key)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	expectedHash := r.URL.Query().Get("request_hash")
	if expectedHash == "" {
		expectedHash = r.URL.Query().Get("hash")
	}
	if expectedHash == "" {
		expectedHash = r.Header.Get("X-Request-Hash")
	}

	if !result.Found {
		api.JSON(w, http.StatusOK, InspectionResponse{
			Key:    key,
			Status: "unknown",
		})
		return
	}

	rec := result.Record

	// If client provided a request_hash to compare against, enforce matching
	// to distinguish valid key reuse from a body mismatch.
	if expectedHash != "" && expectedHash != rec.RequestHash {
		api.Error(w, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED_WITH_DIFFERENT_BODY", "this idempotency key was previously used with a different request body")
		return
	}

	// Optional direct replay for clients requesting raw response
	if (r.URL.Query().Get("raw") == "true" || r.URL.Query().Get("replay") == "true") && rec.Status == StatusComplete {
		writeResponse(w, rec.ResponseStatus, rec.ResponseHeaders, rec.ResponseBody, true)
		return
	}

	resp := InspectionResponse{
		Key:         key,
		CreatedAt:   &rec.CreatedAt,
		ExpiresAt:   &rec.ExpiresAt,
		RequestHash: rec.RequestHash,
	}

	now := h.now()

	switch rec.Status {
	case StatusProcessing:
		resp.Status = "processing"
		retryAfter := minimumRetryAfterSeconds
		if !rec.LeaseExpiresAt.IsZero() && rec.LeaseExpiresAt.After(now) {
			diff := int(math.Ceil(rec.LeaseExpiresAt.Sub(now).Seconds()))
			if diff > retryAfter {
				retryAfter = diff
			}
		}
		resp.RetryAfterSeconds = &retryAfter
		resp.RetryHint = fmt.Sprintf("request is currently processing; retry after %d seconds", retryAfter)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))

	case StatusComplete:
		if rec.ResponseStatus >= 400 {
			resp.Status = "failed"
		} else {
			resp.Status = "completed"
		}

		stored := &StoredResponse{
			Status:        rec.ResponseStatus,
			Headers:       rec.ResponseHeaders,
			BodySizeBytes: len(rec.ResponseBody),
		}

		if len(rec.ResponseBody) > 0 {
			if len(rec.ResponseBody) > MaxInspectionResponseBodyBytes {
				stored.BodyTruncated = true
				stored.BodyText = string(rec.ResponseBody[:MaxInspectionResponseBodyBytes])
			} else {
				if json.Valid(rec.ResponseBody) {
					stored.Body = json.RawMessage(rec.ResponseBody)
				} else {
					stored.BodyText = string(rec.ResponseBody)
				}
			}
		}
		resp.Response = stored

	default:
		resp.Status = "unknown"
	}

	api.JSON(w, http.StatusOK, resp)
}
