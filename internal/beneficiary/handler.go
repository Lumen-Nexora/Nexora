package beneficiary

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
)

type Handler struct{ svc Service }

func NewHandler(svc Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() func(chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.list)
		r.Get("/{id}", h.get)
		r.Post("/", h.create)
		r.Post("/{id}/activate", h.activate)
		r.Delete("/{id}", h.revoke)
	}
}

type createRequest struct {
	Account string `json:"account"`
	Label   string `json:"label,omitempty"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if !canMutate(w, r) {
		return
	}
	var req createRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		api.BadRequest(w, "invalid request body")
		return
	}
	b, err := h.svc.Create(r.Context(), req.Account, req.Label)
	if err != nil {
		h.writeError(w, err)
		return
	}
	api.JSON(w, http.StatusCreated, b)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, map[string]interface{}{"beneficiaries": items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	b, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, b)
}

func (h *Handler) activate(w http.ResponseWriter, r *http.Request) {
	if !canMutate(w, r) {
		return
	}
	b, err := h.svc.Activate(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, b)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if !canMutate(w, r) {
		return
	}
	if err := h.svc.Revoke(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isMutationRole(r *http.Request) bool {
	role := tenant.RoleFromContext(r.Context())
	return strings.EqualFold(role, domain.RoleOwner) || strings.EqualFold(role, domain.RoleAdmin)
}

func canMutate(w http.ResponseWriter, r *http.Request) bool {
	if !isMutationRole(r) {
		api.Error(w, http.StatusForbidden, "FORBIDDEN", "owner or admin role required")
		return false
	}
	if scopes, ok := tenant.ScopesFromContext(r.Context()); ok && !domain.HasScope(scopes, domain.ScopeBeneficiariesWrite) {
		api.Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "API key does not have the required scope: "+domain.ScopeBeneficiariesWrite)
		return false
	}
	return true
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrDuplicate):
		api.Error(w, http.StatusConflict, "BENEFICIARY_EXISTS", err.Error())
	case errors.Is(err, ErrCoolingOff):
		api.Error(w, http.StatusConflict, "COOLING_OFF", err.Error())
	case errors.Is(err, ErrNotActive):
		api.Error(w, http.StatusConflict, "BENEFICIARY_NOT_ACTIVE", err.Error())
	case errors.Is(err, ErrInvalidAccount):
		api.BadRequest(w, err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, domain.ErrForbidden):
		api.Error(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	default:
		api.InternalError(w, err)
	}
}
