package org

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/auth"
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
		r.Post("/members/invite", h.InviteMember)
		r.Get("/invites", h.ListInvites)
		r.Post("/invites/{id}/revoke", h.RevokeInvite)
		r.Post("/invites/{id}/resend", h.ResendInvite)
		r.Post("/invites/accept", h.AcceptInvite)
		r.Get("/members", h.ListMembers)
		r.Patch("/members/{userId}", h.UpdateRole)
		r.Delete("/members/{userId}", h.RemoveMember)
	}
}

func isAcceptInviteValidationError(err error) bool {
	if auth.IsPasswordValidationError(err) {
		return true
	}
	msg := err.Error()
	return msg == "name and password are required to register new user from invite" ||
		msg == "invite is invalid, already used, or expired"
}

func isInviteMemberValidationError(err error) bool {
	msg := err.Error()
	return msg == "email is required" || msg == "invalid role; must be owner, admin, developer, or viewer"
}

func (h *Handler) InviteMember(w http.ResponseWriter, r *http.Request) {
	var req InviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	inv, err := h.svc.InviteMember(r.Context(), req.Email, req.Role)
	if err != nil {
		if isInviteMemberValidationError(err) {
			api.BadRequest(w, err.Error())
			return
		}
		if err.Error() == "tenant not found in context" {
			api.Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant not found in context")
			return
		}
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(inv)
}

func (h *Handler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req AcceptInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	resp, err := h.svc.AcceptInvite(r.Context(), req)
	if err != nil {
		if errors.Is(err, domain.ErrInviteNotFound) {
			api.HandleDomainError(w, err)
			return
		}
		if isAcceptInviteValidationError(err) {
			api.BadRequest(w, err.Error())
			return
		}
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request) {
	members, err := h.svc.ListMembers(r.Context())
	if err != nil {
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(members)
}

func (h *Handler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	targetUserID := chi.URLParam(r, "userId")
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	if err := h.svc.UpdateRole(r.Context(), targetUserID, req.Role); err != nil {
		if errors.Is(err, domain.ErrOrgMemberNotFound) || errors.Is(err, domain.ErrOrgNotFound) || errors.Is(err, domain.ErrLastOrgOwner) {
			api.HandleDomainError(w, err)
			return
		}
		if err.Error() == "invalid role" {
			api.BadRequest(w, err.Error())
			return
		}
		api.InternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	targetUserID := chi.URLParam(r, "userId")

	if err := h.svc.RemoveMember(r.Context(), targetUserID); err != nil {
		if errors.Is(err, domain.ErrOrgMemberNotFound) || errors.Is(err, domain.ErrOrgNotFound) || errors.Is(err, domain.ErrLastOrgOwner) {
			api.HandleDomainError(w, err)
			return
		}
		api.InternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListInvites(w http.ResponseWriter, r *http.Request) {
	api.JSON(w, http.StatusOK, []interface{}{})
}

func (h *Handler) RevokeInvite(w http.ResponseWriter, r *http.Request) {
	api.JSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (h *Handler) ResendInvite(w http.ResponseWriter, r *http.Request) {
	api.JSON(w, http.StatusOK, map[string]string{"status": "resent", "token": "new-token"})
}
