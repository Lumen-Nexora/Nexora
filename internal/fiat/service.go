package fiat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fx"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/Lumen-Nexora/Nexora/internal/transfer"
	"github.com/google/uuid"
)

type Repository interface {
	CreateDeposit(ctx context.Context, d *domain.FiatDeposit) error
	UpdateDepositStatus(ctx context.Context, id, status string) error
	// ClaimDepositForProcessing atomically transitions a deposit from
	// pending to processing, returning an error if it is not currently
	// pending (already claimed by a concurrent/duplicate webhook delivery,
	// or already in a terminal state). Callers must not move funds for a
	// deposit until this succeeds.
	ClaimDepositForProcessing(ctx context.Context, id string) error
	GetDepositByReference(ctx context.Context, ref string) (*domain.FiatDeposit, error)
	CreateWithdrawal(ctx context.Context, w *domain.FiatWithdrawal) error
	UpdateWithdrawalStatus(ctx context.Context, id, status string) error
	GetWithdrawalByReference(ctx context.Context, ref string) (*domain.FiatWithdrawal, error)
}

type WebhookEventRepository interface {
	ClaimWebhookEvent(ctx context.Context, provider, eventID string, expiresAt time.Time) (bool, error)
}

type Service interface {
	GetQuote(ctx context.Context, req QuoteRequest) (*FiatQuote, error)
	InitiateDeposit(ctx context.Context, req DepositRequest) (*DepositResponse, error)
	InitiateWithdrawal(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error)
	// HandleWebhook is the legacy single-signature form (kept for backward
	// compatibility with existing tests and the Rail adapter).
	HandleWebhook(ctx context.Context, payload []byte, signature string) error
	// HandleWebhookWithHeaders is the preferred form used by the HTTP handler:
	// it passes the full request headers so each provider can read its own
	// signature header(s) (e.g. "verif-hash", "x-yellowcard-signature") without
	// the HTTP layer hard-coding provider-specific header names.
	HandleWebhookWithHeaders(ctx context.Context, payload []byte, headers http.Header) error
}

type TenantGetter interface {
	GetByID(ctx context.Context, id string) (*domain.Tenant, error)
}

type service struct {
	repo             Repository
	eventRepo        WebhookEventRepository
	rail             Rail
	fxSvc            fx.Service
	transferSvc      transfer.Service
	platformWalletID string
	providerName     string
	tenantRepo       TenantGetter
}

func NewService(repo Repository, rail Rail, fxSvc fx.Service, transferSvc transfer.Service, platformWalletID, providerName string, eventRepos ...WebhookEventRepository) Service {
	var eventRepo WebhookEventRepository
	if len(eventRepos) > 0 {
		eventRepo = eventRepos[0]
	}
	return &service{
		repo:             repo,
		eventRepo:        eventRepo,
		rail:             rail,
		fxSvc:            fxSvc,
		transferSvc:      transferSvc,
		platformWalletID: platformWalletID,
		providerName:     providerName,
	}
}

func NewServiceWithTenant(repo Repository, rail Rail, fxSvc fx.Service, transferSvc transfer.Service, platformWalletID, providerName string, tenantRepo TenantGetter, eventRepos ...WebhookEventRepository) Service {
	var eventRepo WebhookEventRepository
	if len(eventRepos) > 0 {
		eventRepo = eventRepos[0]
	}
	return &service{
		repo:             repo,
		eventRepo:        eventRepo,
		rail:             rail,
		fxSvc:            fxSvc,
		transferSvc:      transferSvc,
		platformWalletID: platformWalletID,
		providerName:     providerName,
		tenantRepo:       tenantRepo,
	}
}

func (s *service) GetQuote(ctx context.Context, req QuoteRequest) (*FiatQuote, error) {
	return s.rail.GetQuote(ctx, req)
}

// validateFiatCurrency checks code against the rail's supported list and
// returns it normalised (trimmed, upper case). It runs before any pricing so an
// unsupported currency is rejected without touching the rail's quote API.
func (s *service) validateFiatCurrency(code string) (string, error) {
	norm := strings.ToUpper(strings.TrimSpace(code))
	if norm != "" {
		for _, c := range s.rail.SupportedCurrencies() {
			if strings.EqualFold(c, norm) {
				return norm, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %q", domain.ErrUnsupportedFiatCurrency, code)
}

func (s *service) SupportsCurrency(code string) bool {
	_, err := s.validateFiatCurrency(code)
	return err == nil
}

func (s *service) InitiateDeposit(ctx context.Context, req DepositRequest) (*DepositResponse, error) {
	currency, err := s.validateFiatCurrency(req.FiatCurrency)
	if err != nil {
		return nil, err
	}
	req.FiatCurrency = currency

	// Price the deposit with the rail: the user pays fiat and receives USDC.
	// The FX service only quotes Stellar assets, so it cannot price a fiat leg.
	quote, err := s.rail.GetQuote(ctx, QuoteRequest{
		Side:         "deposit",
		FiatCurrency: req.FiatCurrency,
		FiatAmount:   req.FiatAmount,
	})
	if err != nil {
		return nil, fmt.Errorf("get quote for deposit: %w", err)
	}
	if !quote.USDCAmount.IsPositive() {
		return nil, fmt.Errorf("rail returned a non-positive USDC amount for deposit")
	}

	deposit := &domain.FiatDeposit{
		ID:                uuid.New().String(),
		WalletID:          req.WalletID,
		Provider:          s.providerName,
		ProviderReference: req.Reference,
		FiatAmount:        req.FiatAmount,
		FiatCurrency:      req.FiatCurrency,
		USDCAmount:        quote.USDCAmount, // amount of USDC to credit user
		Status:            domain.FiatStatusPending,
		CreatedAt:         time.Now().UTC(),
	}
	if req.PaymentLinkID != "" {
		deposit.PaymentLinkID = &req.PaymentLinkID
	}
	if tenantID := tenant.IDFromContext(ctx); tenantID != "" {
		deposit.TenantID = &tenantID
	}
	if mode, ok := tenant.ModeFromContext(ctx); ok {
		deposit.Mode = mode
	}

	if err := s.repo.CreateDeposit(ctx, deposit); err != nil {
		return nil, fmt.Errorf("create deposit record: %w", err)
	}

	resp, err := s.rail.Deposit(ctx, req)
	if err != nil {
		_ = s.repo.UpdateDepositStatus(ctx, deposit.ID, domain.FiatStatusFailed)
		return nil, fmt.Errorf("rail deposit error: %w", err)
	}

	return resp, nil
}

func (s *service) InitiateWithdrawal(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error) {
	currency, err := s.validateFiatCurrency(req.FiatCurrency)
	if err != nil {
		return nil, err
	}
	req.FiatCurrency = currency

	// Check daily withdrawal limit if tenant repo is available
	tenantID := tenant.IDFromContext(ctx)
	if tenantID != "" && s.tenantRepo != nil {
		t, err := s.tenantRepo.GetByID(ctx, tenantID)
		if err == nil && t != nil {
			dailyLimit := t.GetDailyWithdrawalLimit()
			if dailyLimit > 0 {
				now := time.Now().UTC()
				counter, ok := s.repo.(interface {
					CountDailyWithdrawalsByTenant(context.Context, string, time.Time) (int, error)
				})
				if !ok {
					return nil, errors.New("daily withdrawal limit counter is unavailable")
				}
				count, err := counter.CountDailyWithdrawalsByTenant(ctx, tenantID, now)
				if err != nil {
					return nil, fmt.Errorf("check daily withdrawal limit: %w", err)
				}
				if count >= dailyLimit {
					return nil, domain.ErrDailyWithdrawalLimitReached
				}
			}
		}
	}

	// The user states the fiat amount they want to receive; the rail tells us
	// how much USDC that costs.
	quote, err := s.rail.GetQuote(ctx, QuoteRequest{
		Side:         "withdraw",
		FiatCurrency: req.FiatCurrency,
		FiatAmount:   req.FiatAmount,
	})
	if err != nil {
		return nil, fmt.Errorf("get quote for withdrawal: %w", err)
	}

	usdcAmount := quote.USDCAmount
	if !usdcAmount.IsPositive() {
		return nil, fmt.Errorf("rail returned a non-positive USDC amount for withdrawal")
	}

	withdrawal := &domain.FiatWithdrawal{
		ID:                uuid.New().String(),
		WalletID:          req.WalletID,
		Provider:          s.providerName,
		ProviderReference: req.Reference,
		FiatAmount:        req.FiatAmount,
		FiatCurrency:      req.FiatCurrency,
		USDCAmount:        usdcAmount,
		Status:            domain.FiatStatusPending,
		CreatedAt:         time.Now().UTC(),
	}

	if err := s.repo.CreateWithdrawal(ctx, withdrawal); err != nil {
		return nil, fmt.Errorf("create withdrawal record: %w", err)
	}

	// Debit user wallet, credit platform wallet
	_, err = s.transferSvc.InitiateTransfer(ctx, req.WalletID, s.platformWalletID, "USDC", usdcAmount)
	if err != nil {
		_ = s.repo.UpdateWithdrawalStatus(ctx, withdrawal.ID, domain.FiatStatusFailed)
		return nil, fmt.Errorf("initiate transfer to platform: %w", err)
	}

	resp, err := s.rail.Withdraw(ctx, req)
	if err != nil {
		_ = s.repo.UpdateWithdrawalStatus(ctx, withdrawal.ID, domain.FiatStatusFailed)
		return nil, fmt.Errorf("rail withdraw error: %w", err)
	}

	return resp, nil
}

func (s *service) HandleWebhook(ctx context.Context, payload []byte, signature string) error {
	evt, err := s.rail.HandleWebhook(ctx, payload, signature)
	if err != nil {
		return fmt.Errorf("handle webhook: %w", err)
	}
	return s.processEvent(ctx, evt)
}

// HandleWebhookWithHeaders is the HTTP-handler-facing entry point. It passes
// the full header map to the rail so each provider can read its own signature
// header(s) without the service layer knowing their names.
//
// Errors are wrapped with the appropriate sentinel (ErrWebhookSignatureInvalid,
// ErrWebhookPayloadInvalid, ErrWebhookEventUnknown) when the failure is
// permanent so the HTTP handler can distinguish 4xx from 5xx responses.
func (s *service) HandleWebhookWithHeaders(ctx context.Context, payload []byte, headers http.Header) error {
	evt, err := s.rail.HandleWebhookWithHeaders(ctx, payload, headers)
	if err != nil {
		// Wrap provider-level validation errors so the HTTP handler can map
		// them to 4xx without inspecting the error string.
		if errors.Is(err, ErrWebhookSignatureInvalid) ||
			errors.Is(err, ErrWebhookPayloadInvalid) ||
			errors.Is(err, ErrWebhookEventUnknown) {
			return err
		}
		return fmt.Errorf("handle webhook: %w", err)
	}

	return s.processEvent(ctx, evt)
}

// processEvent applies the business logic for a fully-verified RailEvent.
// It is shared by both HandleWebhook (legacy) and HandleWebhookWithHeaders.
func (s *service) processEvent(ctx context.Context, evt *RailEvent) error {
	if evt.Type == EventDepositConfirmed || evt.Type == EventDepositFailed {
		deposit, err := s.repo.GetDepositByReference(ctx, evt.ProviderRef)
		if err != nil {
			return fmt.Errorf("get deposit by ref: %w", err)
		}
		if deposit.TenantID != nil {
			ctx = tenant.WithID(ctx, *deposit.TenantID)
		}
		if deposit.Mode.Valid() {
			ctx = tenant.WithMode(ctx, deposit.Mode)
		}

		if deposit.Status != domain.FiatStatusPending {
			return nil // already claimed/processed — duplicate or replayed delivery
		}

		if !evt.Amount.Equal(deposit.FiatAmount) || !strings.EqualFold(evt.Currency, deposit.FiatCurrency) {
			return fmt.Errorf(
				"webhook amount/currency mismatch for deposit %s: event has %s %s, expected %s %s",
				deposit.ID, evt.Amount, evt.Currency, deposit.FiatAmount, deposit.FiatCurrency,
			)
		}

		if s.eventRepo != nil && evt.EventID != "" {
			claimed, err := s.eventRepo.ClaimWebhookEvent(ctx, s.providerName, evt.EventID, time.Now().UTC().Add(7*24*time.Hour))
			if err != nil {
				return fmt.Errorf("claim webhook event: %w", err)
			}
			if !claimed {
				return nil
			}
		}

		// Atomically claim the deposit BEFORE moving any funds. This is
		// what makes a concurrent or duplicate webhook delivery for the
		// same event safe: only the caller that wins this pending ->
		// processing transition proceeds to credit the wallet.
		if err := s.repo.ClaimDepositForProcessing(ctx, deposit.ID); err != nil {
			return nil // lost the race, or already handled — idempotent no-op
		}

		if evt.Status == "completed" {
			if _, err := s.transferSvc.InitiateTransfer(ctx, s.platformWalletID, deposit.WalletID, "USDC", deposit.USDCAmount); err != nil {
				_ = s.repo.UpdateDepositStatus(ctx, deposit.ID, domain.FiatStatusFailed)
				return fmt.Errorf("credit user wallet: %w", err)
			}
			if err := s.repo.UpdateDepositStatus(ctx, deposit.ID, domain.FiatStatusCompleted); err != nil {
				return fmt.Errorf("update deposit status: %w", err)
			}
		} else if evt.Status == "failed" {
			if err := s.repo.UpdateDepositStatus(ctx, deposit.ID, domain.FiatStatusFailed); err != nil {
				return fmt.Errorf("update deposit status: %w", err)
			}
		}

	} else if evt.Type == EventWithdrawalSent || evt.Type == EventWithdrawalFailed {
		withdrawal, err := s.repo.GetWithdrawalByReference(ctx, evt.ProviderRef)
		if err != nil {
			return fmt.Errorf("get withdrawal by ref: %w", err)
		}

		if withdrawal.Status != domain.FiatStatusPending {
			return nil
		}

		if s.eventRepo != nil && evt.EventID != "" {
			claimed, err := s.eventRepo.ClaimWebhookEvent(ctx, s.providerName, evt.EventID, time.Now().UTC().Add(7*24*time.Hour))
			if err != nil {
				return fmt.Errorf("claim webhook event: %w", err)
			}
			if !claimed {
				return nil
			}
		}

		if evt.Status == "completed" {
			if err := s.repo.UpdateWithdrawalStatus(ctx, withdrawal.ID, domain.FiatStatusCompleted); err != nil {
				if err.Error() == fmt.Sprintf("withdrawal %s already processed or not pending", withdrawal.ID) {
					return nil
				}
				return fmt.Errorf("update withdrawal status: %w", err)
			}
		} else if evt.Status == "failed" {
			if err := s.repo.UpdateWithdrawalStatus(ctx, withdrawal.ID, domain.FiatStatusFailed); err != nil {
				if err.Error() == fmt.Sprintf("withdrawal %s already processed or not pending", withdrawal.ID) {
					return nil
				}
				return fmt.Errorf("update withdrawal status: %w", err)
			}
			// Refund the user for failed withdrawal
			_, refundErr := s.transferSvc.InitiateTransfer(ctx, s.platformWalletID, withdrawal.WalletID, "USDC", withdrawal.USDCAmount)
			if refundErr != nil {
				return fmt.Errorf("refund user wallet for failed withdrawal: %w", refundErr)
			}
		}
	}

	return nil
}
