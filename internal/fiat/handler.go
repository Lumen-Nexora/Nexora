package fiat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

type Handler struct {
	svc  Service
	idem func(http.Handler) http.Handler
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

// WithIdempotency attaches the idempotency-key middleware to mutating routes.
func (h *Handler) WithIdempotency(mw func(http.Handler) http.Handler) *Handler {
	h.idem = mw
	return h
}

func (h *Handler) DepositRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		post := r.Post
		if h.idem != nil {
			post = r.With(h.idem).Post
		}
		post("/fiat", h.handleDeposit)
	}
}

func (h *Handler) WithdrawRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		post := r.Post
		if h.idem != nil {
			post = r.With(h.idem).Post
		}
		post("/", h.handleWithdrawal)
		post("/fiat", h.handleWithdrawal)
	}
}

func (h *Handler) WebhookRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Post("/{provider}", h.handleWebhook)
	}
}

func (h *Handler) QuoteRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Post("/quote", h.handleQuote)
	}
}

type quoteReq struct {
	Side     string `json:"side" validate:"required,oneof=deposit withdraw"`
	Amount   string `json:"amount" validate:"required"`
	Currency string `json:"currency" validate:"required"`
	Country  string `json:"country" validate:"required"`
}

func (h *Handler) handleQuote(w http.ResponseWriter, r *http.Request) {
	var req quoteReq
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
		api.BadRequest(w, "invalid amount")
		return
	}

	qr := QuoteRequest{
		Side:         req.Side,
		FiatAmount:   amount,
		FiatCurrency: req.Currency,
		Country:      req.Country,
	}

	quote, err := h.svc.GetQuote(r.Context(), qr)
	if err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	api.JSON(w, http.StatusOK, quote)
}

type depositReq struct {
	Amount   string `json:"amount" validate:"required"`
	Currency string `json:"currency" validate:"required"`
	Email    string `json:"email" validate:"required,email"`
	Name     string `json:"name" validate:"required"`
}

func (h *Handler) handleDeposit(w http.ResponseWriter, r *http.Request) {
	walletID := chi.URLParam(r, "id")
	if walletID == "" {
		api.BadRequest(w, "wallet id is required")
		return
	}

	var req depositReq
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
		api.BadRequest(w, "invalid amount")
		return
	}

	dr := DepositRequest{
		WalletID:      walletID,
		Reference:     "DEP-" + uuid.New().String(), // full UUID — 122 bits of entropy
		FiatAmount:    amount,
		FiatCurrency:  req.Currency,
		CustomerEmail: req.Email,
		CustomerName:  req.Name,
	}

	resp, err := h.svc.InitiateDeposit(r.Context(), dr)
	if err != nil {
		if errors.Is(err, domain.ErrUnsupportedFiatCurrency) {
			api.HandleDomainError(w, err)
			return
		}
		log.Error().Err(err).Str("wallet_id", walletID).Msg("initiate deposit failed")
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, resp)
}

type withdrawReq struct {
	WalletID      string `json:"wallet_id,omitempty"`
	Amount        string `json:"amount" validate:"required"`
	Currency      string `json:"currency" validate:"required"`
	AccountBank   string `json:"account_bank" validate:"required"`
	AccountNumber string `json:"account_number" validate:"required"`
}

func (h *Handler) handleWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req withdrawReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	walletID := chi.URLParam(r, "id")
	if walletID == "" {
		walletID = req.WalletID
	}
	if walletID == "" {
		api.BadRequest(w, "wallet id is required")
		return
	}

	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		api.BadRequest(w, "invalid amount")
		return
	}

	wr := WithdrawRequest{
		WalletID:      walletID,
		Reference:     "WIT-" + uuid.New().String(), // full UUID — 122 bits of entropy
		FiatAmount:    amount,
		FiatCurrency:  req.Currency,
		AccountBank:   req.AccountBank,
		AccountNumber: req.AccountNumber,
	}

	resp, err := h.svc.InitiateWithdrawal(r.Context(), wr)
	if err != nil {
		if errors.Is(err, domain.ErrUnsupportedFiatCurrency) {
			api.HandleDomainError(w, err)
			return
		}
		log.Error().Err(err).Str("wallet_id", walletID).Msg("initiate withdrawal failed")
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, resp)
}

// webhookCallbackDTO is the minimal shape every provider callback must satisfy.
// Individual providers do their own full decode after the handler validates this.
type webhookCallbackDTO struct {
	Event string `json:"event" validate:"required"`
}

// handleWebhook handles inbound provider callbacks.
//
// Error classification (important for provider retry behaviour):
//   - 4xx: the payload is permanently invalid (bad signature, unknown event
//     type, missing required fields). Providers should NOT retry these.
//   - 5xx: a transient infrastructure failure occurred (DB down, transfer
//     service unavailable). Providers SHOULD retry after a delay.
//
// Access control is HMAC signature verification performed by the provider
// implementation, not by API-key authentication. The route is therefore
// mounted in the public (unauthenticated) sub-router in server.go.
func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if provider == "" {
		api.BadRequest(w, "provider is required")
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		// Body read failure is transient — return 5xx so the provider retries.
		log.Error().Err(err).Str("provider", provider).Msg("failed to read webhook body")
		api.InternalError(w, err)
		return
	}

	// Validate the outer structure so a completely malformed body is rejected
	// immediately with 4xx before the provider layer even inspects it.
	var dto webhookCallbackDTO
	if err := json.Unmarshal(payload, &dto); err != nil {
		api.BadRequest(w, "webhook payload must be valid JSON with an 'event' field")
		return
	}
	if err := api.Validate(dto); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	// Pass the raw headers to the service so provider-specific signature
	// headers (e.g. "verif-hash" for Flutterwave, "x-yellowcard-signature"
	// for Yellow Card) are forwarded without loss.
	if err := h.svc.HandleWebhookWithHeaders(r.Context(), payload, r.Header); err != nil {
		log.Error().Err(err).Str("provider", provider).Msg("webhook handling failed")
		if errors.Is(err, ErrWebhookSignatureInvalid) ||
			errors.Is(err, ErrWebhookPayloadInvalid) ||
			errors.Is(err, ErrWebhookEventUnknown) {
			// Permanent rejection: bad signature, unrecognisable payload, or
			// an event type this provider does not support. Providers must not
			// retry these — the same payload will fail again.
			api.BadRequest(w, err.Error())
			return
		}
		// Transient failure (DB unavailable, transfer service down, etc.).
		// Return 5xx so the provider retries after its back-off delay.
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
