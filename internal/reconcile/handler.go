package reconcile

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc   *Service
	audit interface {
		Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{})
	}
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) WithAuditLogger(audit interface {
	Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{})
}) *Handler {
	h.audit = audit
	return h
}

func (h *Handler) AdminRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/reconciliation/summary", h.summary)
		r.Get("/reconciliation/drift", h.drift)
		r.Get("/reconciliation/discrepancies", h.discrepancies)
		r.Get("/reconciliation/discrepancies/summary", h.discrepancySummary)
		r.Patch("/reconciliation/discrepancies/{id}", h.updateDiscrepancy)
		r.Post("/reconciliation/run", h.run)
		r.Post("/transfers/{transferID}/force-settle", h.forceSettle)
		r.Post("/reconcile/wallet/{walletID}/run", h.runReconcile)
	}
}

func (h *Handler) discrepancies(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.IDFromContext(r.Context())
	if tenantID == "" {
		api.Error(w, http.StatusUnauthorized, "TENANT_REQUIRED", "tenant context is required")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	if status != "" && status != "open" && status != "acknowledged" && status != "resolved" {
		api.BadRequest(w, "status must be open, acknowledged, or resolved")
		return
	}
	if category != "" && !validDiscrepancyCategory(category) {
		api.BadRequest(w, "unsupported discrepancy category")
		return
	}
	items, total, err := h.svc.ListDiscrepancies(r.Context(), tenantID, status, category, limit, offset)
	if err != nil {
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{
		"discrepancies": items, "total": total, "limit": normalizedDiscrepancyLimit(limit), "offset": max(offset, 0),
	})
}

func (h *Handler) discrepancySummary(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.IDFromContext(r.Context())
	if tenantID == "" {
		api.Error(w, http.StatusUnauthorized, "TENANT_REQUIRED", "tenant context is required")
		return
	}
	summary, err := h.svc.DiscrepancySummary(r.Context(), tenantID)
	if err != nil {
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, summary)
}

func (h *Handler) updateDiscrepancy(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.IDFromContext(r.Context())
	if tenantID == "" {
		api.Error(w, http.StatusUnauthorized, "TENANT_REQUIRED", "tenant context is required")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		api.BadRequest(w, "id must be a valid UUID")
		return
	}
	var req struct {
		Action     string `json:"action"`
		Note       string `json:"note"`
		AssignedTo string `json:"assigned_to"`
	}
	if err := decodeDiscrepancyAction(r, &req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	req.Action = strings.TrimSpace(strings.ToLower(req.Action))
	req.Note = strings.TrimSpace(req.Note)
	if req.Action != "acknowledged" && req.Action != "assigned" && req.Action != "annotated" && req.Action != "resolved" {
		api.BadRequest(w, "action must be acknowledged, assigned, annotated, or resolved")
		return
	}
	if (req.Action == "annotated" || req.Action == "resolved") && req.Note == "" {
		api.BadRequest(w, "note is required for annotation and resolution")
		return
	}
	if req.Action == "assigned" {
		if _, err := uuid.Parse(req.AssignedTo); err != nil {
			api.BadRequest(w, "assigned_to must be a valid user UUID")
			return
		}
	} else if req.AssignedTo != "" {
		api.BadRequest(w, "assigned_to is only valid for the assigned action")
		return
	}
	d, err := h.svc.UpdateDiscrepancy(r.Context(), tenantID, id, req.Action, req.Note, req.AssignedTo)
	if err != nil {
		if errors.Is(err, domain.ErrConcurrentUpdate) {
			api.Error(w, http.StatusConflict, "DISCREPANCY_STATE_CONFLICT", "discrepancy is resolved or has already changed")
			return
		}
		api.InternalError(w, err)
		return
	}
	if h.audit != nil {
		h.audit.Log(r, "reconciliation.discrepancy."+req.Action, "reconciliation_discrepancy", id, map[string]interface{}{"category": d.Category})
	}
	api.JSON(w, http.StatusOK, d)
}

func decodeDiscrepancyAction(r *http.Request, dst interface{}) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

func validDiscrepancyCategory(category string) bool {
	switch category {
	case domain.DiscrepancyMissingOnChain, domain.DiscrepancyAmountMismatch, domain.DiscrepancyAssetMismatch, domain.DiscrepancyDuplicateSettlement, domain.DiscrepancyStalePending:
		return true
	default:
		return false
	}
}

func normalizedDiscrepancyLimit(limit int) int {
	if limit < 1 || limit > 100 {
		return 50
	}
	return limit
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 && d <= 90 {
			days = d
		}
	}

	summary, err := h.svc.GetSummary(r.Context(), days)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, summary)
}

func (h *Handler) drift(w http.ResponseWriter, r *http.Request) {
	snapshots, err := h.svc.GetDrift(r.Context())
	if err != nil {
		api.InternalError(w, err)
		return
	}
	// Always emit a JSON array, never null, so clients can iterate the result
	// without a nil check when there is no drift.
	if snapshots == nil {
		snapshots = []*DriftSnapshot{}
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{
		"drift":   snapshots,
		"count":   len(snapshots),
		"checked": time.Now().UTC(),
	})
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RunAll(r.Context()); err != nil {
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusAccepted, map[string]interface{}{"status": "triggered"})
}

func (h *Handler) forceSettle(w http.ResponseWriter, r *http.Request) {
	transferID := chi.URLParam(r, "transferID")
	if transferID == "" {
		api.Error(w, http.StatusBadRequest, "TRANSFER_ID_REQUIRED", "transferID is required")
		return
	}
	if _, err := uuid.Parse(transferID); err != nil {
		api.Error(w, http.StatusBadRequest, "TRANSFER_ID_INVALID", "transferID must be a valid UUID")
		return
	}

	actor := api.ActorFromContext(r.Context())
	if err := h.svc.EnqueueForceSettle(r.Context(), transferID, actor); err != nil {
		if errors.Is(err, domain.ErrConcurrentUpdate) {
			api.Error(w, http.StatusConflict, "CONFLICT", "transfer is not in a retryable state")
			return
		}
		api.WriteError(w, r, err)
		return
	}
	if h.audit != nil {
		h.audit.Log(r, "transfer.retry", "transfer", transferID, map[string]interface{}{"operation": "retry_failed_settlement"})
	}

	api.JSON(w, http.StatusAccepted, map[string]interface{}{"status": "retry_queued", "transfer_id": transferID})
}

func (h *Handler) runReconcile(w http.ResponseWriter, r *http.Request) {
	walletID := chi.URLParam(r, "walletID")
	if walletID == "" {
		api.Error(w, http.StatusBadRequest, "WALLET_ID_REQUIRED", "walletID is required")
		return
	}

	actor := api.ActorFromContext(r.Context())
	if err := h.svc.EnqueueWalletReconcile(r.Context(), walletID, actor); err != nil {
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{"status": "enqueued", "wallet_id": walletID})
}
