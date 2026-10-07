package treasury

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
)

type Handler struct {
	svc   Service
	guard func(http.Handler) http.Handler
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

// WithMutationGate attaches middleware (e.g. a role check) to the
// state-mutating routes only (POST /sweep, PUT /config) — treasury moves
// real money, so these get the same Owner/Admin-only gating already applied
// to /v1/keys and /v1/org, unlike the read routes here.
func (h *Handler) WithMutationGate(mw func(http.Handler) http.Handler) *Handler {
	h.guard = mw
	return h
}

// AdminRoutes is mounted at /v1/admin/treasury.
func (h *Handler) AdminRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/balances", h.getBalances)
		r.Get("/reserve", h.getReserve)
		r.Get("/sweeps", h.listSweeps)
		r.Get("/config", h.getConfig)

		post, put := r.Post, r.Put
		if h.guard != nil {
			post = r.With(h.guard).Post
			put = r.With(h.guard).Put
		}
		post("/sweep", h.manualSweep)
		put("/config", h.updateConfig)
	}
}

func (h *Handler) getBalances(w http.ResponseWriter, r *http.Request) {
	balances, err := h.svc.GetBalances(r.Context())
	if err != nil {
		api.InternalError(w, err)
		return
	}

	type balanceResponse struct {
		Asset         string `json:"asset"`
		Balance       string `json:"balance"`
		USDEquivalent string `json:"usd_equivalent"`
	}
	resp := make([]balanceResponse, len(balances))
	for i, b := range balances {
		resp[i] = balanceResponse{Asset: b.Asset, Balance: b.Balance.StringFixed(7), USDEquivalent: b.USDEquivalent.StringFixed(2)}
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"balances": resp})
}

func (h *Handler) getReserve(w http.ResponseWriter, r *http.Request) {
	breakdown, err := h.svc.GetReserveBreakdown(r.Context())
	if err != nil {
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{
		"wallet_count":        breakdown.WalletCount,
		"trustline_count":     breakdown.TrustlineCount,
		"open_offers_count":   breakdown.OfferCount,
		"total_xlm_required":  breakdown.TotalXLMRequired.StringFixed(7),
		"current_xlm_balance": breakdown.CurrentXLMBalance.StringFixed(7),
		"surplus":             breakdown.Surplus.StringFixed(7),
	})
}

func (h *Handler) listSweeps(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	sweeps, err := h.svc.ListSweeps(r.Context(), limit, offset)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	type sweepResponse struct {
		ID          string `json:"id"`
		Asset       string `json:"asset"`
		Amount      string `json:"amount"`
		Destination string `json:"destination"`
		TxHash      string `json:"tx_hash"`
		TriggeredBy string `json:"triggered_by"`
		SweptAt     string `json:"swept_at"`
	}
	resp := make([]sweepResponse, len(sweeps))
	for i, s := range sweeps {
		resp[i] = sweepResponse{
			ID: s.ID, Asset: s.Asset, Amount: s.Amount.StringFixed(7),
			Destination: s.Destination, TxHash: s.TxHash, TriggeredBy: s.TriggeredBy,
			SweptAt: s.SweptAt.Format(time.RFC3339),
		}
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"sweeps": resp})
}

type manualSweepRequest struct {
	Asset       string `json:"asset"       validate:"required"`
	Amount      string `json:"amount"      validate:"required"`
	Destination string `json:"destination" validate:"required"`
}

func (h *Handler) manualSweep(w http.ResponseWriter, r *http.Request) {
	var req manualSweepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		api.BadRequest(w, "amount must be a positive number")
		return
	}

	actor := "admin"
	if userID := r.Context().Value("user_id"); userID != nil {
		if idStr, ok := userID.(string); ok && idStr != "" {
			actor = idStr
		}
	}
	txHash, err := h.svc.ExecuteSweep(r.Context(), req.Asset, amount, req.Destination, actor)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"asset":       req.Asset,
		"amount":      amount.StringFixed(7),
		"destination": req.Destination,
		"tx_hash":     txHash,
	})
}

func (h *Handler) getConfig(w http.ResponseWriter, r *http.Request) {
	configs, err := h.svc.GetConfig(r.Context())
	if err != nil {
		api.InternalError(w, err)
		return
	}

	type configResponse struct {
		Asset              string `json:"asset"`
		SweepThreshold     string `json:"sweep_threshold"`
		MinOperatingBuffer string `json:"min_operating_buffer"`
		ColdStorageAddress string `json:"cold_storage_address"`
		AutoSweepEnabled   bool   `json:"auto_sweep_enabled"`
	}
	resp := make([]configResponse, len(configs))
	for i, c := range configs {
		resp[i] = configResponse{
			Asset: c.Asset, SweepThreshold: c.SweepThreshold.StringFixed(7),
			MinOperatingBuffer: c.MinOperatingBuffer.StringFixed(7),
			ColdStorageAddress: c.ColdStorageAddress, AutoSweepEnabled: c.AutoSweepEnabled,
		}
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"config": resp})
}

type updateConfigRequest struct {
	Asset              string `json:"asset"                 validate:"required"`
	SweepThreshold     string `json:"sweep_threshold"       validate:"required"`
	MinOperatingBuffer string `json:"min_operating_buffer"  validate:"required"`
	ColdStorageAddress string `json:"cold_storage_address"`
	AutoSweepEnabled   bool   `json:"auto_sweep_enabled"`
}

func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req updateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	sweepThreshold, err := decimal.NewFromString(req.SweepThreshold)
	if err != nil || sweepThreshold.IsNegative() {
		api.BadRequest(w, "sweep_threshold must be a non-negative number")
		return
	}
	minBuffer, err := decimal.NewFromString(req.MinOperatingBuffer)
	if err != nil || minBuffer.IsNegative() {
		api.BadRequest(w, "min_operating_buffer must be a non-negative number")
		return
	}

	cfg := &Config{
		Asset:              req.Asset,
		SweepThreshold:     sweepThreshold,
		MinOperatingBuffer: minBuffer,
		ColdStorageAddress: req.ColdStorageAddress,
		AutoSweepEnabled:   req.AutoSweepEnabled,
	}
	tenantID := r.Context().Value("tenant_id")
	if tenantID != nil && tenantID != "platform" && tenantID != "system" && tenantID != "operator" {
		api.Error(w, http.StatusForbidden, "FORBIDDEN", "tenants cannot modify global treasury configuration")
		return
	}
	if err := h.svc.UpdateConfig(r.Context(), cfg); err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"asset":                cfg.Asset,
		"sweep_threshold":      cfg.SweepThreshold.StringFixed(7),
		"min_operating_buffer": cfg.MinOperatingBuffer.StringFixed(7),
		"cold_storage_address": cfg.ColdStorageAddress,
		"auto_sweep_enabled":   cfg.AutoSweepEnabled,
	})
}
