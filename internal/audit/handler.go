package audit

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
)

type Handler struct {
	svc Service
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.List)
		r.Get("/export", h.Export)
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	filter := parseFilter(r)
	events, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"events": events,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	filter := parseFilter(r)
	format := r.URL.Query().Get("format")
	if format == "" {
		if strings.Contains(r.Header.Get("Accept"), "text/csv") {
			format = "csv"
		} else {
			format = "json"
		}
	}

	data, contentType, err := h.svc.Export(r.Context(), filter, format)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", contentType)
	if format == "csv" {
		w.Header().Set("Content-Disposition", "attachment; filename=\"audit-log.csv\"")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func parseFilter(r *http.Request) domain.AuditFilter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	filter := domain.AuditFilter{
		Action:       q.Get("action"),
		ActorID:      q.Get("actor"),
		ResourceType: q.Get("resource_type"),
		ResourceID:   q.Get("resource_id"),
		Limit:        limit,
		Offset:       offset,
	}

	if fromStr := q.Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			filter.From = &t
		}
	}
	if toStr := q.Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			filter.To = &t
		}
	}

	return filter
}
