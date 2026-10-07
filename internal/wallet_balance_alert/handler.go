package wallet_balance_alert

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
)

type Handler struct{ svc Service }

func NewHandler(svc Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() func(chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.list)
		r.Get("/{id}", h.get)
		r.Post("/", h.create)
		r.Put("/{id}", h.update)
		r.Delete("/{id}", h.delete)
		r.Get("/{id}/events", h.listEvents)
	}
}

type createRequest struct {
	WalletID    string `json:"wallet_id" validate:"required"`
	AssetCode   string `json:"asset_code" validate:"required"`
	AssetIssuer string `json:"asset_issuer,omitempty"`
	Threshold   string `json:"threshold" validate:"required"`
}

type updateRequest struct {
	Threshold string                          `json:"threshold,omitempty"`
	Status    domain.WalletBalanceAlertStatus `json:"status,omitempty"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	a, err := h.svc.Create(r.Context(), req.WalletID, req.AssetCode, req.AssetIssuer, req.Threshold)
	if err != nil {
		h.writeError(w, err)
		return
	}

	api.JSON(w, http.StatusCreated, a)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, a)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	walletID := r.URL.Query().Get("wallet_id")
	items, err := h.svc.List(r.Context(), walletID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"alerts": items})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	a, err := h.svc.Update(r.Context(), id, req.Threshold, req.Status)
	if err != nil {
		h.writeError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, a)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	events, err := h.svc.ListEvents(r.Context(), id, limit)
	if err != nil {
		h.writeError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{"events": events})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrWalletBalanceAlertNotFound):
		api.Error(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrDuplicateAlert):
		api.Error(w, http.StatusConflict, "ALERT_EXISTS", err.Error())
	case errors.Is(err, ErrInvalidThreshold):
		api.BadRequest(w, err.Error())
	case errors.Is(err, ErrInvalidAsset):
		api.BadRequest(w, err.Error())
	case errors.Is(err, domain.ErrForbidden):
		api.Error(w, http.StatusForbidden, "FORBIDDEN", err.Error())
	default:
		api.InternalError(w, err)
	}
}
