package wallet

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
)

// MemoPolicyHandler serves the tenant-wide memo policy routes and the queue
// of deposits whose memo could not be mapped to a customer.
type MemoPolicyHandler struct {
	svc *MemoPolicyService
}

func NewMemoPolicyHandler(svc *MemoPolicyService) *MemoPolicyHandler {
	return &MemoPolicyHandler{svc: svc}
}

// PolicyRoutes registers the routes under /v1/memo-policies.
func (h *MemoPolicyHandler) PolicyRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.listPolicies)
	}
}

// DepositRoutes registers the routes under /v1/unmapped-deposits.
func (h *MemoPolicyHandler) DepositRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.listUnmappedDeposits)
		r.Post("/{id}/resolve", h.resolveUnmappedDeposit)
	}
}

// memoPolicyRequest is the body of a memo policy write. The booleans are
// pointers so that an omitted field keeps the documented default instead of
// silently disabling the policy.
type memoPolicyRequest struct {
	Format             string `json:"format"`
	Pattern            string `json:"pattern,omitempty"`
	RequireMemo        *bool  `json:"require_memo,omitempty"`
	AutoRefund         *bool  `json:"auto_refund,omitempty"`
	RefundAfterSeconds *int64 `json:"refund_after_seconds,omitempty"`
	Enabled            *bool  `json:"enabled,omitempty"`
}

// memoPolicyResponse renders a policy with the refund timeout expressed in
// seconds, which is how the API accepts it.
func memoPolicyResponse(policy *domain.MemoPolicy) map[string]interface{} {
	return map[string]interface{}{
		"id":                   policy.ID,
		"tenant_id":            policy.TenantID,
		"wallet_id":            policy.WalletID,
		"format":               string(policy.Format),
		"pattern":              policy.Pattern,
		"require_memo":         policy.RequireMemo,
		"auto_refund":          policy.AutoRefund,
		"refund_after_seconds": int64(policy.RefundAfter / time.Second),
		"enabled":              policy.Enabled,
		"created_at":           policy.CreatedAt,
		"updated_at":           policy.UpdatedAt,
	}
}

func (h *MemoPolicyHandler) listPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.svc.ListPolicies(r.Context())
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	items := make([]map[string]interface{}, 0, len(policies))
	for _, policy := range policies {
		items = append(items, memoPolicyResponse(policy))
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"memo_policies": items})
}

func (h *MemoPolicyHandler) listUnmappedDeposits(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	offset := 0
	if value := r.URL.Query().Get("offset"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	walletID := strings.TrimSpace(r.URL.Query().Get("wallet_id"))

	deposits, total, err := h.svc.ListUnmappedDeposits(r.Context(), walletID, status, limit, offset)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{
		"data":   deposits,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *MemoPolicyHandler) resolveUnmappedDeposit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Resolution string `json:"resolution"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			api.BadRequest(w, "invalid request body")
			return
		}
	}
	if strings.TrimSpace(req.Resolution) == "" {
		api.BadRequest(w, "a resolution note is required")
		return
	}
	deposit, err := h.svc.ResolveUnmappedDeposit(r.Context(), chi.URLParam(r, "id"), req.Resolution)
	if err != nil {
		writeMemoPolicyError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, deposit)
}

// writeMemoPolicyError maps the memo policy errors onto HTTP statuses. The
// package-level errors are not part of the domain error catalogue, so they
// are translated here instead of in the shared API helper.
func writeMemoPolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidMemoPolicy):
		api.BadRequest(w, err.Error())
	case errors.Is(err, ErrUnmappedDepositNotPending):
		api.Error(w, http.StatusConflict, "UNMAPPED_DEPOSIT_NOT_PENDING", err.Error())
	default:
		api.WriteError(w, r, err)
	}
}
