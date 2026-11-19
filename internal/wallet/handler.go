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

type Handler struct {
	svc          Service
	simulation   SimulationService
	contractSvc  ContractService
	memoSvc      *MemoPolicyService
	idem         func(http.Handler) http.Handler
	guardianGate func(http.Handler) http.Handler
	closer       *Closer
	closureGate  func(http.Handler) http.Handler
	consolidator *Consolidator
	consolidGate func(http.Handler) http.Handler
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

// WithContractService enables the contract-wallet endpoints. They are only
// registered when a contract adapter is wired in, so a deployment running
// purely custodial wallets does not expose routes it cannot serve.
func (h *Handler) WithContractService(contractSvc ContractService) *Handler {
	h.contractSvc = contractSvc
	return h
}

// WithMemoPolicyService enables the inbound memo policy endpoints of a
// wallet. They are only registered when a memo policy service is wired in,
// so a deployment that does not enforce memo rules does not expose routes
// it cannot serve.
func (h *Handler) WithMemoPolicyService(svc *MemoPolicyService) *Handler {
	h.memoSvc = svc
	return h
}

// WithIdempotency attaches the idempotency-key middleware to the
// state-mutating routes (POST / and POST /{id}/trustlines) only.
func (h *Handler) WithIdempotency(mw func(http.Handler) http.Handler) *Handler {
	h.idem = mw
	return h
}

// WithGuardianGate attaches middleware (e.g. a role check) to the
// guardian and time-lock mutation routes only (POST/DELETE /{id}/guardians,
// POST /{id}/time-lock). These control the contract wallet's recovery and
// spending-freeze mechanisms, so they get the same Owner/Admin-only gating
// applied to /v1/keys and /v1/org.
func (h *Handler) WithGuardianGate(mw func(http.Handler) http.Handler) *Handler {
	h.guardianGate = mw
	return h
}

func (h *Handler) WithSimulation(simulation SimulationService) *Handler {
	h.simulation = simulation
	return h
}

func (h *Handler) simulate(w http.ResponseWriter, r *http.Request) {
	var req SimulationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		api.BadRequest(w, "invalid simulation request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		api.BadRequest(w, "expected one request object")
		return
	}
	result, err := h.simulation.Simulate(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, result)
}

// WithCloser enables the destructive account closure and custody settings
// endpoints. gate (e.g. an Owner/Admin role check) wraps every one of them,
// including the reads, because the settings name the tenant's treasury
// accounts.
func (h *Handler) WithCloser(closer *Closer, gate func(http.Handler) http.Handler) *Handler {
	h.closer = closer
	h.closureGate = gate
	return h
}

// WithConsolidator enables the multi-wallet consolidation endpoints behind
// gate (e.g. an Owner/Admin role check), since they move funds between the
// tenant's wallets and can close accounts.
func (h *Handler) WithConsolidator(c *Consolidator, gate func(http.Handler) http.Handler) *Handler {
	h.consolidator = c
	h.consolidGate = gate
	return h
}

func (h *Handler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		post := r.Post
		if h.idem != nil {
			post = r.With(h.idem).Post
		}
		post("/", h.createWallet)
		r.Get("/", h.listWallets)
		r.Get("/{id}", h.getWallet)
		r.Get("/{id}/balances", h.getBalances)
		post("/{id}/trustlines", h.addTrustline)
		if h.simulation != nil {
			r.Post("/{id}/simulate", h.simulate)
		}

		if h.closer != nil {
			gated := r.With()
			if h.closureGate != nil {
				gated = r.With(h.closureGate)
			}
			closePost := gated.Post
			if h.idem != nil {
				closePost = gated.With(h.idem).Post
			}
			closePost("/{id}/close", h.closeWallet)
			gated.Get("/closures", h.listClosures)
			gated.Get("/closures/{closureId}", h.getClosure)
			gated.Get("/custody-settings", h.getCustodySettings)
			gated.Put("/custody-settings", h.putCustodySettings)
		}

		if h.consolidator != nil {
			gated := r.With()
			if h.consolidGate != nil {
				gated = r.With(h.consolidGate)
			}
			gated.Post("/consolidations", h.createConsolidation)
			gated.Get("/consolidations", h.listConsolidations)
			gated.Get("/consolidations/{consolidationId}", h.getConsolidation)
		}

		if h.memoSvc != nil {
			r.Get("/{id}/memo-policy", h.getMemoPolicy)
			r.Put("/{id}/memo-policy", h.putMemoPolicy)
			r.Delete("/{id}/memo-policy", h.deleteMemoPolicy)
		}

		if h.contractSvc != nil {
			r.Get("/{id}/contract-state", h.getContractState)
			r.Get("/{id}/spending-status", h.getSpendingStatus)

			guardianPost, guardianDelete := r.Post, r.Delete
			if h.guardianGate != nil {
				guardianPost = r.With(h.guardianGate).Post
				guardianDelete = r.With(h.guardianGate).Delete
			}
			guardianPost("/{id}/guardians", h.addGuardian)
			guardianDelete("/{id}/guardians/{address}", h.removeGuardian)
			guardianPost("/{id}/time-lock", h.setTimeLock)
		}
	}
}

type addTrustlineRequest struct {
	Asset  string `json:"asset" validate:"required"`
	Issuer string `json:"issuer,omitempty"`
	Limit  string `json:"limit,omitempty"`
}

type createWalletRequest struct {
	// OwnerPublicKey switches wallet creation to the non-custodial contract
	// adapter. When omitted, a custodial wallet is created.
	OwnerPublicKey string `json:"owner_public_key,omitempty"`
}

type addGuardianRequest struct {
	Address string `json:"address" validate:"required"`
}

type setTimeLockRequest struct {
	UntilTimestamp uint64 `json:"untilTimestamp"`
}

func (h *Handler) listWallets(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	wallets, err := h.svc.ListWallets(r.Context(), limit, offset)
	if err != nil {
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"wallets": wallets})
}

func (h *Handler) getWallet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Use service's repository directly via GetBalances path to avoid exposing secret
	// For now fetch via balances repo; we need wallet details.
	// Service doesn't expose GetByID, so we reuse GetBalances with empty FX to validate existence,
	// then fetch wallet via repo if needed. Simplest: try to load balances and return wallet ID.
	// Instead, we will ask the service if it can load the wallet by attempting to get balances.
	// Fallback: return the ID as public_key if not found in stellar.
	wallet, err := h.svc.GetWalletForHandler(r.Context(), id)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{
		"id":           wallet.ID,
		"public_key":   wallet.PublicKey,
		"mode":         wallet.Mode,
		"custody_type": wallet.CustodyType,
		"created_at":   wallet.CreatedAt,
	})
}

func (h *Handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var req createWalletRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			api.BadRequest(w, "invalid request body")
			return
		}
	}

	svc := h.svc
	var owner []string
	if req.OwnerPublicKey != "" {
		if h.contractSvc == nil {
			api.BadRequest(w, "contract wallets are not enabled on this deployment")
			return
		}
		svc = h.contractSvc
		owner = []string{req.OwnerPublicKey}
	}

	wallet, err := svc.CreateWallet(r.Context(), owner...)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	resp := map[string]interface{}{
		"id":           wallet.ID,
		"public_key":   wallet.PublicKey,
		"mode":         wallet.Mode,
		"custody_type": wallet.CustodyType,
		"created_at":   wallet.CreatedAt,
	}
	if wallet.ContractID != "" {
		resp["contract_id"] = wallet.ContractID
	}

	api.JSON(w, http.StatusCreated, resp)
}

func (h *Handler) getContractState(w http.ResponseWriter, r *http.Request) {
	state, err := h.contractSvc.GetContractState(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, state)
}

func (h *Handler) getSpendingStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.contractSvc.GetSpendingStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, status)
}

func (h *Handler) addGuardian(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req addGuardianRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	txHash, err := h.contractSvc.AddGuardian(r.Context(), id, req.Address)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id": id,
		"guardian":  req.Address,
		"tx_hash":   txHash,
	})
}

func (h *Handler) removeGuardian(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	address := chi.URLParam(r, "address")

	txHash, err := h.contractSvc.RemoveGuardian(r.Context(), id, address)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id": id,
		"guardian":  address,
		"tx_hash":   txHash,
	})
}

func (h *Handler) setTimeLock(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req setTimeLockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	txHash, err := h.contractSvc.SetTimeLock(r.Context(), id, req.UntilTimestamp)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id":       id,
		"until_timestamp": req.UntilTimestamp,
		"tx_hash":         txHash,
	})
}

func (h *Handler) getBalances(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	includeFX := r.URL.Query().Get("include_fx")

	balances, err := h.svc.GetBalances(r.Context(), id, includeFX)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id": id,
		"balances":  balances,
	})
}

func (h *Handler) addTrustline(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req addTrustlineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	txHash, err := h.svc.AddTrustline(r.Context(), id, req.Asset, req.Issuer, req.Limit)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"status":    "confirmed",
		"wallet_id": id,
		"asset":     req.Asset,
		"tx_hash":   txHash,
	})
}

// getMemoPolicy returns the inbound memo policy of a wallet.
func (h *Handler) getMemoPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := h.memoSvc.GetPolicy(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, memoPolicyResponse(policy))
}

// putMemoPolicy creates or replaces the inbound memo policy of a wallet.
func (h *Handler) putMemoPolicy(w http.ResponseWriter, r *http.Request) {
	var req memoPolicyRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			api.BadRequest(w, "invalid request body")
			return
		}
	}

	policy := &domain.MemoPolicy{
		WalletID:    chi.URLParam(r, "id"),
		Format:      domain.MemoFormat(strings.ToLower(strings.TrimSpace(req.Format))),
		Pattern:     strings.TrimSpace(req.Pattern),
		RequireMemo: true,
		AutoRefund:  false,
		Enabled:     true,
	}
	if req.RequireMemo != nil {
		policy.RequireMemo = *req.RequireMemo
	}
	if req.AutoRefund != nil {
		policy.AutoRefund = *req.AutoRefund
	}
	if req.Enabled != nil {
		policy.Enabled = *req.Enabled
	}
	if req.RefundAfterSeconds != nil {
		if *req.RefundAfterSeconds < 0 {
			api.BadRequest(w, "refund_after_seconds cannot be negative")
			return
		}
		policy.RefundAfter = time.Duration(*req.RefundAfterSeconds) * time.Second
	}

	stored, err := h.memoSvc.PutPolicy(r.Context(), policy)
	if err != nil {
		writeMemoPolicyError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, memoPolicyResponse(stored))
}

// deleteMemoPolicy removes the inbound memo policy of a wallet.
func (h *Handler) deleteMemoPolicy(w http.ResponseWriter, r *http.Request) {
	if err := h.memoSvc.DeletePolicy(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeMemoPolicyError(w, r, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{
		"status":    "deleted",
		"wallet_id": chi.URLParam(r, "id"),
	})
}
