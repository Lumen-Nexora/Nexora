package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
)

type TravelRuleConfirmationService interface {
	HandleConfirmation(ctx context.Context, confirmation domain.TravelRuleConfirmation) error
}

type TravelRuleHandler struct {
	service TravelRuleConfirmationService
}

func NewTravelRuleHandler(service TravelRuleConfirmationService) *TravelRuleHandler {
	return &TravelRuleHandler{service: service}
}

// Routes is mounted outside tenant authentication. The service verifies the
// recipient VASP's signature before any transfer status can change.
func (h *TravelRuleHandler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Post("/confirmations", h.confirmation)
	}
}

func (h *TravelRuleHandler) confirmation(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var confirmation domain.TravelRuleConfirmation
	if err := decoder.Decode(&confirmation); err != nil {
		api.BadRequest(w, "invalid Travel Rule confirmation")
		return
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		api.BadRequest(w, "invalid Travel Rule confirmation")
		return
	}
	if err := h.service.HandleConfirmation(r.Context(), confirmation); err != nil {
		if errors.Is(err, ErrInvalidTravelRuleConfirmation) {
			api.Error(w, http.StatusUnauthorized, "INVALID_TRAVEL_RULE_CONFIRMATION", "confirmation could not be verified")
			return
		}
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
