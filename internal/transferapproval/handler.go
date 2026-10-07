package transferapproval

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, total, err := h.service.List(r.Context(), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"data": items, "total": total, "limit": limit, "offset": offset})
}
func (h *Handler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := h.service.GetPolicy(r.Context(), r.URL.Query().Get("asset"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if p == nil {
		api.NotFound(w, "approval policy not found")
		return
	}
	api.JSON(w, http.StatusOK, policyResponse(p))
}
func (h *Handler) PutPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Asset     string `json:"asset"`
		Threshold string `json:"threshold"`
		Required  int    `json:"required_approvals"`
		Expires   int64  `json:"expires_after_seconds"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	amount, err := decimal.NewFromString(body.Threshold)
	if err != nil {
		api.BadRequest(w, "threshold must be a decimal string")
		return
	}
	p := &domain.TransferApprovalPolicy{Asset: body.Asset, Threshold: amount, RequiredApprovals: body.Required, ExpiresAfter: time.Duration(body.Expires) * time.Second, Enabled: body.Enabled}
	if err := h.service.PutPolicy(r.Context(), p); err != nil {
		if errors.Is(err, ErrInvalidPolicy) {
			api.BadRequest(w, err.Error())
			return
		}
		api.WriteError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, policyResponse(p))
}

func policyResponse(p *domain.TransferApprovalPolicy) map[string]interface{} {
	return map[string]interface{}{"tenant_id": p.TenantID, "asset": p.Asset, "threshold": p.Threshold, "required_approvals": p.RequiredApprovals, "expires_after_seconds": int64(p.ExpiresAfter / time.Second), "enabled": p.Enabled, "updated_at": p.UpdatedAt}
}
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) { h.decide(w, r, "approved") }
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request)  { h.decide(w, r, "rejected") }
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, decision string) {
	var body struct {
		Note string `json:"note"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	result, err := h.service.Decide(r.Context(), chi.URLParam(r, "id"), decision, body.Note)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, result)
}
