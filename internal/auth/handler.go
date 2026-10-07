package auth

import (
	"encoding/json"
	"errors"
	"net/http"

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
		r.Post("/register", h.Register)
		r.Post("/login", h.Login)
		r.Post("/refresh", h.Refresh)
	}
}

func isRegisterValidationError(err error) bool {
	if IsPasswordValidationError(err) {
		return true
	}
	msg := err.Error()
	return msg == "email, password, and name are required" ||
		msg == "account_type must be 'individual' or 'organization'" ||
		msg == "org_name is required for organization account registration"
}

func isInvalidRefreshTokenError(err error) bool {
	return err.Error() == "invalid or expired refresh token" || err.Error() == "tenant not found" ||
		errors.Is(err, domain.ErrUserNotFound) || errors.Is(err, domain.ErrOrgMemberNotFound)
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	resp, err := h.svc.Register(r.Context(), req)
	if err != nil {
		if errors.Is(err, domain.ErrUserAlreadyExists) {
			api.Error(w, http.StatusConflict, "CONFLICT", err.Error())
			return
		}
		if isRegisterValidationError(err) {
			api.BadRequest(w, err.Error())
			return
		}
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	resp, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidCredentials) {
			api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	resp, err := h.svc.RefreshToken(r.Context(), req.RefreshToken)
	if err != nil {
		if isInvalidRefreshTokenError(err) {
			api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired refresh token")
			return
		}
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
