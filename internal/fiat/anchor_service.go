package fiat

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/anchor"
	"github.com/Lumen-Nexora/Nexora/internal/crypto"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
)

// WalletGetter is the narrow slice of wallet.Repository the anchor fiat
// service needs: the wallet's Stellar keypair to run SEP-10 auth as the
// depositing/withdrawing account.
type WalletGetter interface {
	GetByID(ctx context.Context, id string) (*domain.Wallet, error)
}

// AnchorRepository is the anchor.Repository slice needed to persist
// Nexora's own record of deposits/withdrawals initiated against anchors.
type AnchorRepository interface {
	CreateTransaction(ctx context.Context, t *domain.AnchorTransaction) error
	GetTransactionByID(ctx context.Context, id string, tenantID *string) (*domain.AnchorTransaction, error)
	UpdateTransactionStatus(ctx context.Context, id, status string, completedAt *time.Time, tenantID *string) error
}

// AnchorRegistry is the slice of anchor.Registry the fiat service needs to
// resolve which anchor handles a given asset.
type AnchorRegistry interface {
	ResolveForAsset(assetCode string) (*domain.Anchor, error)
	GetByID(id string) (*domain.Anchor, error)
}

// AnchorFiatService implements the unified /v1/fiat/* endpoints: it resolves
// the correct SEP-compliant anchor for an asset, authenticates with SEP-10,
// and initiates a SEP-6 non-interactive transfer or a SEP-24 interactive one
// depending on what the anchor supports.
type AnchorFiatService struct {
	registry       AnchorRegistry
	repo           AnchorRepository
	walletRepo     WalletGetter
	masterKey      []byte
	stellarNetwork string
	httpClient     *http.Client
}

func NewAnchorFiatService(registry AnchorRegistry, repo AnchorRepository, walletRepo WalletGetter, masterKey []byte, stellarNetwork string) *AnchorFiatService {
	return &AnchorFiatService{
		registry:       registry,
		repo:           repo,
		walletRepo:     walletRepo,
		masterKey:      masterKey,
		stellarNetwork: stellarNetwork,
		httpClient:     &http.Client{Timeout: 20 * time.Second},
	}
}

type AnchorDepositRequest struct {
	WalletID  string
	AssetCode string
	Amount    string
	Email     string
}

type AnchorWithdrawRequest struct {
	WalletID  string
	AssetCode string
	Amount    string
	Dest      string
}

// AnchorTransferResult is returned by InitiateDeposit/InitiateWithdrawal:
// either a SEP-6 set of bank instructions or a SEP-24 interactive URL,
// alongside Nexora's own transaction ID for polling via GetTransaction.
type AnchorTransferResult struct {
	Type           string // "sep6" | "sep24"
	Instructions   *anchor.DepositInstructions
	Withdrawal     *anchor.WithdrawalResult
	InteractiveURL string
	TransactionID  string
}

func (s *AnchorFiatService) InitiateDeposit(ctx context.Context, req AnchorDepositRequest) (*AnchorTransferResult, error) {
	a, wallet, secretKey, err := s.resolveAndAuthPrereqs(ctx, req.AssetCode, req.WalletID)
	if err != nil {
		return nil, err
	}

	jwt, err := s.authenticate(ctx, a, wallet.PublicKey, secretKey)
	if err != nil {
		return nil, err
	}

	txID := uuid.New().String()
	now := time.Now().UTC()

	if a.TransferServer != "" {
		instr, err := anchor.NewSep6Client(s.httpClient).InitiateDeposit(ctx, a.TransferServer, jwt, req.AssetCode, wallet.PublicKey, req.Amount, req.Email)
		if err != nil {
			return nil, fmt.Errorf("sep-6 deposit: %w", err)
		}

		record := &domain.AnchorTransaction{
			ID: txID, WalletID: wallet.ID, AnchorID: a.ID, ExternalTxID: instr.ID,
			Asset: req.AssetCode, Amount: req.Amount, Type: domain.AnchorTxTypeDeposit,
			Status: "incomplete", CreatedAt: now,
		}
		if err := s.repo.CreateTransaction(ctx, record); err != nil {
			return nil, fmt.Errorf("record deposit: %w", err)
		}

		return &AnchorTransferResult{Type: "sep6", Instructions: instr, TransactionID: txID}, nil
	}

	if a.TransferServerSep24 != "" {
		interactiveURL, externalID, err := anchor.NewSep24Client(s.httpClient).GetInteractiveUrl(ctx, a.TransferServerSep24, jwt, req.AssetCode, wallet.PublicKey, req.Amount, domain.AnchorTxTypeDeposit)
		if err != nil {
			return nil, fmt.Errorf("sep-24 deposit: %w", err)
		}

		record := &domain.AnchorTransaction{
			ID: txID, WalletID: wallet.ID, AnchorID: a.ID, ExternalTxID: externalID,
			Asset: req.AssetCode, Amount: req.Amount, Type: domain.AnchorTxTypeDeposit,
			Status: "incomplete", CreatedAt: now,
		}
		if err := s.repo.CreateTransaction(ctx, record); err != nil {
			return nil, fmt.Errorf("record deposit: %w", err)
		}

		return &AnchorTransferResult{Type: "sep24", InteractiveURL: interactiveURL, TransactionID: txID}, nil
	}

	return nil, fmt.Errorf("anchor %s supports neither SEP-6 nor SEP-24", a.HomeDomain)
}

func (s *AnchorFiatService) InitiateWithdrawal(ctx context.Context, req AnchorWithdrawRequest) (*AnchorTransferResult, error) {
	a, wallet, secretKey, err := s.resolveAndAuthPrereqs(ctx, req.AssetCode, req.WalletID)
	if err != nil {
		return nil, err
	}

	jwt, err := s.authenticate(ctx, a, wallet.PublicKey, secretKey)
	if err != nil {
		return nil, err
	}

	txID := uuid.New().String()
	now := time.Now().UTC()

	if a.TransferServer != "" {
		result, err := anchor.NewSep6Client(s.httpClient).InitiateWithdrawal(ctx, a.TransferServer, jwt, req.AssetCode, req.Amount, req.Dest)
		if err != nil {
			return nil, fmt.Errorf("sep-6 withdrawal: %w", err)
		}

		record := &domain.AnchorTransaction{
			ID: txID, WalletID: wallet.ID, AnchorID: a.ID, ExternalTxID: result.ID,
			Asset: req.AssetCode, Amount: req.Amount, Type: domain.AnchorTxTypeWithdrawal,
			Status: "pending_user_transfer_start", CreatedAt: now,
		}
		if err := s.repo.CreateTransaction(ctx, record); err != nil {
			return nil, fmt.Errorf("record withdrawal: %w", err)
		}

		return &AnchorTransferResult{Type: "sep6", Withdrawal: result, TransactionID: txID}, nil
	}

	if a.TransferServerSep24 != "" {
		interactiveURL, externalID, err := anchor.NewSep24Client(s.httpClient).GetInteractiveUrl(ctx, a.TransferServerSep24, jwt, req.AssetCode, wallet.PublicKey, req.Amount, domain.AnchorTxTypeWithdrawal)
		if err != nil {
			return nil, fmt.Errorf("sep-24 withdrawal: %w", err)
		}

		record := &domain.AnchorTransaction{
			ID: txID, WalletID: wallet.ID, AnchorID: a.ID, ExternalTxID: externalID,
			Asset: req.AssetCode, Amount: req.Amount, Type: domain.AnchorTxTypeWithdrawal,
			Status: "incomplete", CreatedAt: now,
		}
		if err := s.repo.CreateTransaction(ctx, record); err != nil {
			return nil, fmt.Errorf("record withdrawal: %w", err)
		}

		return &AnchorTransferResult{Type: "sep24", InteractiveURL: interactiveURL, TransactionID: txID}, nil
	}

	return nil, fmt.Errorf("anchor %s supports neither SEP-6 nor SEP-24", a.HomeDomain)
}

// GetTransaction polls the owning anchor for a transaction's current status,
// updates Nexora's own record if it changed, and returns the normalised
// record (whose Status mirrors the anchor's SEP-6/24 status verbatim).
func (s *AnchorFiatService) GetTransaction(ctx context.Context, id string) (*domain.AnchorTransaction, error) {
	tenantID := tenant.IDFromContext(ctx)
	var tenantPtr *string
	if tenantID != "" {
		tenantPtr = &tenantID
	}

	record, err := s.repo.GetTransactionByID(ctx, id, tenantPtr)
	if err != nil {
		return nil, err
	}

	a, err := s.registry.GetByID(record.AnchorID)
	if err != nil {
		return nil, err
	}

	wallet, err := s.walletRepo.GetByID(ctx, record.WalletID)
	if err != nil {
		return nil, fmt.Errorf("get wallet: %w", err)
	}
	secretKey, err := s.decryptSecret(wallet.EncryptedSecret)
	if err != nil {
		return nil, err
	}
	jwt, err := s.authenticate(ctx, a, wallet.PublicKey, secretKey)
	if err != nil {
		return nil, err
	}

	var remote *anchor.AnchorTransaction
	if a.TransferServer != "" {
		remote, err = anchor.NewSep6Client(s.httpClient).GetTransaction(ctx, a.TransferServer, jwt, record.ExternalTxID)
	} else {
		remote, err = anchor.NewSep24Client(s.httpClient).PollTransaction(ctx, a.TransferServerSep24, jwt, record.ExternalTxID)
	}
	if err != nil {
		return nil, fmt.Errorf("poll anchor transaction: %w", err)
	}

	if remote.Status != "" && remote.Status != record.Status {
		var completedAt *time.Time
		if remote.Status == "completed" {
			now := time.Now().UTC()
			completedAt = &now
		}
		if err := s.repo.UpdateTransactionStatus(ctx, record.ID, remote.Status, completedAt, tenantPtr); err != nil {
			return nil, fmt.Errorf("update transaction status: %w", err)
		}
		record.Status = remote.Status
		record.CompletedAt = completedAt
	}

	return record, nil
}

func (s *AnchorFiatService) resolveAndAuthPrereqs(ctx context.Context, assetCode, walletID string) (*domain.Anchor, *domain.Wallet, string, error) {
	a, err := s.registry.ResolveForAsset(assetCode)
	if err != nil {
		return nil, nil, "", err
	}

	wallet, err := s.walletRepo.GetByID(ctx, walletID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("get wallet: %w", err)
	}

	secretKey, err := s.decryptSecret(wallet.EncryptedSecret)
	if err != nil {
		return nil, nil, "", err
	}

	return a, wallet, secretKey, nil
}

func (s *AnchorFiatService) authenticate(ctx context.Context, a *domain.Anchor, publicKey, secretKey string) (string, error) {
	networkPassphrase := a.NetworkPassphrase
	if networkPassphrase == "" {
		networkPassphrase = anchor.NetworkPassphrase(s.stellarNetwork)
	}

	sep10 := anchor.NewSep10Client(a.WebAuthEndpoint, a.HomeDomain, networkPassphrase, s.httpClient)
	challenge, err := sep10.Challenge(ctx, a.Sep10SigningKey, publicKey)
	if err != nil {
		return "", fmt.Errorf("sep-10 challenge: %w", err)
	}
	jwt, err := sep10.Authenticate(ctx, challenge, secretKey)
	if err != nil {
		return "", fmt.Errorf("sep-10 authenticate: %w", err)
	}
	return jwt, nil
}

func (s *AnchorFiatService) decryptSecret(encryptedSecretHex string) (string, error) {
	ciphertext, err := hex.DecodeString(encryptedSecretHex)
	if err != nil {
		return "", fmt.Errorf("decode wallet secret: %w", err)
	}
	plaintext, err := crypto.Decrypt(ciphertext, s.masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt wallet secret: %w", err)
	}
	return string(plaintext), nil
}
