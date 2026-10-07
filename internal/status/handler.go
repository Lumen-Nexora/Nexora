package status

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/health"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes registers the unauthenticated platform status endpoints on
// the root API router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/status", h.GetStatus)
	r.Get("/status/incidents", h.ListIncidents)
	r.Get("/status/dependencies/history", h.ListDependencyHistory)
}

// RegisterAdminRoutes must be mounted inside the authenticated Owner/Admin
// group and therefore produces /v1/admin/incidents.
func (h *Handler) RegisterAdminRoutes(r chi.Router) {
	r.Post("/admin/incidents", h.CreateIncident)
	r.Patch("/admin/incidents/{id}", h.UpdateIncident)
}

func (h *Handler) GetStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.service.GetStatus(r.Context())
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, res)
}

func (h *Handler) ListIncidents(w http.ResponseWriter, r *http.Request) {
	incidents, err := h.service.ListIncidents(r.Context())
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]any{"incidents": incidents})
}

func (h *Handler) CreateIncident(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	inc, err := h.service.CreateIncident(r.Context(), req)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusCreated, inc)
}

func (h *Handler) UpdateIncident(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		api.BadRequest(w, "incident id must be a valid UUID")
		return
	}

	var req domain.UpdateIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	inc, err := h.service.UpdateIncident(r.Context(), id, req)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, inc)
}

func (h *Handler) ListDependencyHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	dependency := query.Get("dependency")
	if dependency != "" && !dependencyNamePattern.MatchString(dependency) {
		api.BadRequest(w, "invalid dependency name")
		return
	}
	var since *time.Time
	if raw := query.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			api.BadRequest(w, "since must be an RFC3339 timestamp")
			return
		}
		parsed = parsed.UTC()
		since = &parsed
	}
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > health.HistoryMaxLimit {
			api.BadRequest(w, "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}
	checks, err := h.service.ListDependencyHistory(r.Context(), dependency, since, limit)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, DependencyHistoryResponse{Checks: checks, Limit: limit, RetentionDays: int(health.HistoryRetention / (24 * time.Hour))})
}
