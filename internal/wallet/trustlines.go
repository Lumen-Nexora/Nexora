package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
)

// Stellar's base reserve is 0.5 XLM per ledger entry on every network.
const baseReserveXLM = 0.5

var (
	// ErrTrustlineMissing means the account has no trustline for the asset.
	ErrTrustlineMissing = errors.New("trustline is missing")
	// ErrTrustlineReserve means the account cannot fund the 0.5 XLM reserve a
	// new trustline requires.
	ErrTrustlineReserve = errors.New("insufficient XLM reserve to create trustline")
	// ErrTrustlineUnauthorized means the issuer requires (or has revoked)
	// authorization for the asset. SEP-8 regulated assets surface this way.
	ErrTrustlineUnauthorized = errors.New("asset issuer authorization required")
	// ErrTrustlineLimitExceeded means the transfer would push the balance past
	// the trustline limit.
	ErrTrustlineLimitExceeded = errors.New("trustline limit exceeded")
)

// TrustlineError adds the Stellar result code to a trustline failure so
// callers can distinguish op_low_reserve from op_not_authorized and friends.
type TrustlineError struct {
	Code string
	Err  error
}

func (e *TrustlineError) Error() string {
	if e.Code == "" {
		return e.Err.Error()
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *TrustlineError) Unwrap() error { return e.Err }

// TrustlineStatus is a read-only snapshot of one wallet/asset trustline.
type TrustlineStatus struct {
	AssetCode                       string `json:"asset_code"`
	Issuer                          string `json:"issuer"`
	Exists                          bool   `json:"exists"`
	Authorized                      bool   `json:"authorized"`
	AuthorizedToMaintainLiabilities bool   `json:"authorized_to_maintain_liabilities"`
	Limit                           string `json:"limit,omitempty"`
	Balance                         string `json:"balance,omitempty"`
	IssuerAuthRequired              bool   `json:"issuer_auth_required"`
	IssuerAuthRevocable             bool   `json:"issuer_auth_revocable"`
	IssuerAuthImmutable             bool   `json:"issuer_auth_immutable"`
	// SEP8 marks assets whose issuer controls access via authorization flags,
	// the on-chain signal SEP-8 regulated assets use for approval.
	SEP8 bool `json:"sep8"`
}

// TrustlineService verifies and, when needed, creates trustlines before a
// transfer. It is additive to Service so existing implementations keep working.
type TrustlineService interface {
	EnsureTrustline(ctx context.Context, walletID, assetCode, issuer string, amount decimal.Decimal) (string, error)
	TrustlineStatus(ctx context.Context, walletID, assetCode, issuer string) (*TrustlineStatus, error)
}

func (s *service) resolveIssuer(assetCode, issuer string) (string, error) {
	if assetCode == "" || strings.EqualFold(assetCode, "XLM") {
		return "", nil
	}
	if issuer != "" {
		return issuer, nil
	}
	if a, ok := s.assetRegistry.Get(assetCode); ok && a.Issuer != "" {
		return a.Issuer, nil
	}
	switch strings.ToUpper(assetCode) {
	case "USDC":
		issuer = s.usdcIssuer
	case "EURC":
		issuer = s.eurcIssuer
	}
	if issuer == "" {
		return "", fmt.Errorf("%w: missing issuer for asset %s", domain.ErrInvalidAsset, assetCode)
	}
	return issuer, nil
}

// EnsureTrustline verifies the wallet can receive assetCode and creates the
// trustline when it is missing and the account can afford the reserve. A
// non-empty hash means a ChangeTrust was submitted.
func (s *service) EnsureTrustline(ctx context.Context, walletID, assetCode, issuer string, amount decimal.Decimal) (string, error) {
	if assetCode == "" || strings.EqualFold(assetCode, "XLM") {
		return "", nil
	}

	issuer, err := s.resolveIssuer(assetCode, issuer)
	if err != nil {
		return "", err
	}

	w, err := s.repo.GetByID(ctx, walletID)
	if err != nil {
		return "", err
	}

	acct, err := stellar.LoadAccountWithContext(ctx, s.client(ctx), w.PublicKey)
	if err != nil {
		if stellar.IsNotFound(err) {
			return "", fmt.Errorf("%w: account %s is not funded", ErrTrustlineMissing, w.PublicKey)
		}
		return "", fmt.Errorf("load account: %w", err)
	}

	if bal, ok := findTrustline(acct, assetCode, issuer); ok {
		return "", usableTrustline(bal, amount)
	}

	flags, _ := s.issuerFlags(ctx, issuer)
	if native, err := decimal.NewFromString(nativeBalance(acct)); err == nil {
		required := minimumXLM(acct.SubentryCount + 1)
		if native.LessThan(required) {
			return "", fmt.Errorf("%w: need %s XLM, have %s", ErrTrustlineReserve, required, native)
		}
	}

	hash, err := s.submitChangeTrust(ctx, &acct, w.EncryptedSecret, assetCode, issuer, txnbuild.MaxTrustlineLimit)
	if err != nil {
		return "", mapTrustlineSubmitError(err)
	}

	updated, err := stellar.LoadAccountWithContext(ctx, s.client(ctx), w.PublicKey)
	if err != nil {
		return hash, fmt.Errorf("verify trustline: %w", err)
	}
	bal, ok := findTrustline(updated, assetCode, issuer)
	if !ok {
		return hash, &TrustlineError{Code: "verify", Err: ErrTrustlineMissing}
	}
	if !isAuthorized(bal) {
		return hash, &TrustlineError{Code: "op_not_authorized", Err: fmt.Errorf("%w (separable=%t)", ErrTrustlineUnauthorized, flags.AuthRequired)}
	}
	return hash, nil
}

func (s *service) TrustlineStatus(ctx context.Context, walletID, assetCode, issuer string) (*TrustlineStatus, error) {
	issuer, err := s.resolveIssuer(assetCode, issuer)
	if err != nil {
		return nil, err
	}
	w, err := s.repo.GetByID(ctx, walletID)
	if err != nil {
		return nil, err
	}
	acct, err := stellar.LoadAccountWithContext(ctx, s.client(ctx), w.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("load account: %w", err)
	}

	status := &TrustlineStatus{AssetCode: assetCode, Issuer: issuer}
	if bal, ok := findTrustline(acct, assetCode, issuer); ok {
		status.Exists = true
		status.Authorized = isAuthorized(bal)
		status.AuthorizedToMaintainLiabilities = isAuthorizedToMaintain(bal)
		status.Limit = bal.Limit
		status.Balance = bal.Balance
	}
	if flags, ok := s.issuerFlags(ctx, issuer); ok {
		status.IssuerAuthRequired = flags.AuthRequired
		status.IssuerAuthRevocable = flags.AuthRevocable
		status.IssuerAuthImmutable = flags.AuthImmutable
		status.SEP8 = flags.AuthRequired || flags.AuthRevocable
	}
	return status, nil
}

func (s *service) issuerFlags(ctx context.Context, issuer string) (horizon.AccountFlags, bool) {
	if issuer == "" {
		return horizon.AccountFlags{}, false
	}
	acct, err := stellar.LoadAccountWithContext(ctx, s.client(ctx), issuer)
	if err != nil {
		return horizon.AccountFlags{}, false
	}
	return acct.Flags, true
}

func (s *service) submitChangeTrust(ctx context.Context, acct *horizon.Account, encryptedSecret, assetCode, issuer, limit string) (string, error) {
	ctAsset, err := txnbuild.CreditAsset{Code: assetCode, Issuer: issuer}.ToChangeTrustAsset()
	if err != nil {
		return "", fmt.Errorf("build change trust asset: %w", err)
	}
	if limit == "" {
		limit = txnbuild.MaxTrustlineLimit
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        acct,
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&txnbuild.ChangeTrust{Line: ctAsset, Limit: limit}},
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(30)},
	})
	if err != nil {
		return "", fmt.Errorf("build change trust transaction: %w", err)
	}
	signed, err := s.sign(ctx, tx, encryptedSecret)
	if err != nil {
		return "", fmt.Errorf("sign trustline transaction: %w", err)
	}
	resp, err := stellar.SubmitTransactionWithContext(ctx, s.client(ctx), signed)
	if err != nil {
		return "", err
	}
	return resp.Hash, nil
}

// mapTrustlineSubmitError translates Horizon operation result codes into the
// trustline sentinels the API layer can act on.
func mapTrustlineSubmitError(err error) error {
	var herr *horizonclient.Error
	if !errors.As(err, &herr) {
		return &TrustlineError{Code: "submit", Err: err}
	}
	codes, codeErr := herr.ResultCodes()
	if codeErr != nil || codes == nil {
		return &TrustlineError{Code: "submit", Err: err}
	}
	for _, code := range codes.OperationCodes {
		switch code {
		case "op_low_reserve":
			return &TrustlineError{Code: code, Err: fmt.Errorf("%w: %v", ErrTrustlineReserve, err)}
		case "op_not_authorized":
			return &TrustlineError{Code: code, Err: fmt.Errorf("%w: %v", ErrTrustlineUnauthorized, err)}
		case "op_invalid_limit", "op_malformed":
			return &TrustlineError{Code: code, Err: fmt.Errorf("%w: %v", ErrTrustlineLimitExceeded, err)}
		}
	}
	return &TrustlineError{Code: codes.TransactionCode, Err: err}
}

func findTrustline(acct horizon.Account, assetCode, issuer string) (horizon.Balance, bool) {
	for _, bal := range acct.Balances {
		if strings.EqualFold(bal.Code, assetCode) && bal.Issuer == issuer {
			return bal, true
		}
	}
	return horizon.Balance{}, false
}

func isAuthorized(bal horizon.Balance) bool {
	return bal.IsAuthorized == nil || *bal.IsAuthorized
}

func isAuthorizedToMaintain(bal horizon.Balance) bool {
	return bal.IsAuthorizedToMaintainLiabilities != nil && *bal.IsAuthorizedToMaintainLiabilities
}

func usableTrustline(bal horizon.Balance, amount decimal.Decimal) error {
	if !isAuthorized(bal) {
		return &TrustlineError{Code: "op_not_authorized", Err: ErrTrustlineUnauthorized}
	}
	if amount.LessThanOrEqual(decimal.Zero) {
		return nil
	}
	limit, err := decimal.NewFromString(bal.Limit)
	if err != nil || limit.IsZero() {
		return nil
	}
	balance, err := decimal.NewFromString(bal.Balance)
	if err != nil {
		return nil
	}
	liabilities, _ := decimal.NewFromString(bal.SellingLiabilities)
	if balance.Add(liabilities).Add(amount).GreaterThan(limit) {
		return &TrustlineError{
			Code: "op_invalid_limit",
			Err:  fmt.Errorf("%w: balance %s + liabilities %s + amount %s > limit %s", ErrTrustlineLimitExceeded, balance, liabilities, amount, limit),
		}
	}
	return nil
}

func nativeBalance(acct horizon.Account) string {
	balance, err := acct.GetNativeBalance()
	if err != nil {
		return "0"
	}
	return balance
}

func minimumXLM(subentries int32) decimal.Decimal {
	if subentries < 0 {
		subentries = 0
	}
	return decimal.NewFromFloat(baseReserveXLM).Mul(decimal.NewFromInt(int64(2 + subentries)))
}
