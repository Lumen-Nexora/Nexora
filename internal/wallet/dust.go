package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
)

const stroopsPerXLM = 10_000_000

var (
	ErrDustTreasuryRequired = errors.New("dust treasury account is required")
	ErrDustNoRate           = errors.New("no USD rate available for asset")
)

// DustConfig controls detection and consolidation of micro balances.
type DustConfig struct {
	// ThresholdUSD defines dust: a balance worth less than this is sweepable.
	ThresholdUSD decimal.Decimal
	// TreasuryAccount receives the consolidated stablecoin.
	TreasuryAccount string
	// TreasuryWalletID, when set, receives the cached balance credit.
	TreasuryWalletID    string
	TreasuryAssetCode   string
	TreasuryAssetIssuer string
	MaxOpsPerTx         int
	BaseFeeStroops      int64
	SlippageTolerance   decimal.Decimal
	PageSize            int
	MaxWallets          int
	DryRun              bool
}

func (c DustConfig) withDefaults() DustConfig {
	if c.ThresholdUSD.LessThanOrEqual(decimal.Zero) {
		c.ThresholdUSD = decimal.NewFromFloat(0.01)
	}
	if c.TreasuryAssetCode == "" {
		c.TreasuryAssetCode = "USDC"
	}
	if c.MaxOpsPerTx <= 0 {
		c.MaxOpsPerTx = 100
	}
	if c.BaseFeeStroops <= 0 {
		c.BaseFeeStroops = txnbuild.MinBaseFee
	}
	if c.SlippageTolerance.LessThanOrEqual(decimal.Zero) || c.SlippageTolerance.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		c.SlippageTolerance = decimal.NewFromFloat(0.005)
	}
	if c.PageSize <= 0 || c.PageSize > 200 {
		c.PageSize = 100
	}
	return c
}

// DustBalance is one balance classified by the scanner.
type DustBalance struct {
	WalletID   string          `json:"wallet_id"`
	PublicKey  string          `json:"public_key"`
	AssetCode  string          `json:"asset_code"`
	Issuer     string          `json:"issuer,omitempty"`
	Amount     decimal.Decimal `json:"amount"`
	USDValue   decimal.Decimal `json:"usd_value"`
	FeeUSD     decimal.Decimal `json:"fee_usd"`
	NetUSD     decimal.Decimal `json:"net_usd"`
	Sweepable  bool            `json:"sweepable"`
	SkipReason string          `json:"skip_reason,omitempty"`
}

// DustSweep records one consolidated operation.
type DustSweep struct {
	WalletID    string          `json:"wallet_id"`
	AssetCode   string          `json:"asset_code"`
	Issuer      string          `json:"issuer,omitempty"`
	Amount      decimal.Decimal `json:"amount"`
	Collected   decimal.Decimal `json:"collected"`
	TxHash      string          `json:"tx_hash"`
	PathPayment bool            `json:"path_payment"`
}

// DustReport is the operational summary of a scan or sweep.
type DustReport struct {
	ScannedWallets      int             `json:"scanned_wallets"`
	DustBalances        []DustBalance   `json:"dust_balances"`
	Sweeps              []DustSweep     `json:"sweeps"`
	SkippedUneconomical int             `json:"skipped_uneconomical"`
	Failed              int             `json:"failed"`
	TotalDustCollected  decimal.Decimal `json:"total_dust_collected"`
	TotalFeesConsumed   decimal.Decimal `json:"total_fees_consumed_xlm"`
	TotalFeesUSD        decimal.Decimal `json:"total_fees_usd"`
	Errors              []string        `json:"errors,omitempty"`
	DryRun              bool            `json:"dry_run"`
}

// ScanDust identifies dust balances without submitting anything.
func (s *service) ScanDust(ctx context.Context, cfg DustConfig) (*DustReport, error) {
	return s.runDust(ctx, cfg, false)
}

// SweepDust consolidates dust into the configured treasury stablecoin. Set
// cfg.DryRun to get the same report without broadcasting.
func (s *service) SweepDust(ctx context.Context, cfg DustConfig) (*DustReport, error) {
	return s.runDust(ctx, cfg, !cfg.DryRun)
}

func (s *service) runDust(ctx context.Context, cfg DustConfig, execute bool) (*DustReport, error) {
	cfg = cfg.withDefaults()
	if execute && cfg.TreasuryAccount == "" {
		return nil, ErrDustTreasuryRequired
	}

	report := &DustReport{DustBalances: []DustBalance{}, Sweeps: []DustSweep{}, DryRun: !execute}
	xlmUSD, hasXLMRate := s.usdRate(ctx, "XLM")

	tenantID := tenant.IDFromContext(ctx)
	offset := 0
	for {
		wallets, err := s.repo.List(ctx, cfg.PageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("list wallets: %w", err)
		}
		for _, w := range wallets {
			if cfg.MaxWallets > 0 && report.ScannedWallets >= cfg.MaxWallets {
				return report, nil
			}
			if tenantID != "" && w.TenantID != nil && *w.TenantID != tenantID {
				continue
			}
			report.ScannedWallets++
			if err := s.scanWalletDust(ctx, cfg, w, xlmUSD, hasXLMRate, execute, report); err != nil {
				report.Failed++
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", w.ID, err))
			}
		}
		if len(wallets) < cfg.PageSize {
			break
		}
		offset += len(wallets)
	}
	return report, nil
}

func (s *service) scanWalletDust(ctx context.Context, cfg DustConfig, w *domain.Wallet, xlmUSD decimal.Decimal, hasXLMRate, execute bool, report *DustReport) error {
	acct, err := stellar.LoadAccountWithContext(ctx, s.client(ctx), w.PublicKey)
	if err != nil {
		if stellar.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("load account: %w", err)
	}

	var dust []DustBalance
	for _, bal := range acct.Balances {
		code := bal.Code
		if code == "" {
			code = "XLM"
		}
		amount, err := decimal.NewFromString(bal.Balance)
		if err != nil || amount.LessThanOrEqual(decimal.Zero) {
			continue
		}
		if strings.EqualFold(code, "XLM") {
			amount = amount.Sub(minimumXLM(acct.SubentryCount))
			if amount.LessThanOrEqual(decimal.Zero) {
				continue
			}
		}

		usd, ok := s.valueUSD(ctx, code, amount, xlmUSD, hasXLMRate)
		if !ok {
			continue
		}
		if usd.GreaterThanOrEqual(cfg.ThresholdUSD) {
			continue
		}

		feeUSD := s.feeUSD(cfg.BaseFeeStroops, xlmUSD, hasXLMRate)
		entry := DustBalance{
			WalletID:  w.ID,
			PublicKey: w.PublicKey,
			AssetCode: code,
			Issuer:    bal.Issuer,
			Amount:    amount,
			USDValue:  usd,
			FeeUSD:    feeUSD,
			NetUSD:    usd.Sub(feeUSD),
			Sweepable: usd.GreaterThan(feeUSD),
		}
		if !entry.Sweepable {
			entry.SkipReason = "network fee exceeds dust value"
			report.SkippedUneconomical++
		}
		report.DustBalances = append(report.DustBalances, entry)
		dust = append(dust, entry)
	}

	if !execute {
		return nil
	}

	sweepable := make([]DustBalance, 0, len(dust))
	for _, d := range dust {
		if d.Sweepable {
			sweepable = append(sweepable, d)
		}
	}
	if len(sweepable) == 0 {
		return nil
	}
	return s.sweepWalletDust(ctx, cfg, w, &acct, sweepable, report)
}

func (s *service) sweepWalletDust(ctx context.Context, cfg DustConfig, w *domain.Wallet, acct *horizon.Account, dust []DustBalance, report *DustReport) error {
	treasuryAsset, err := s.treasuryAsset(cfg)
	if err != nil {
		return err
	}

	for start := 0; start < len(dust); start += cfg.MaxOpsPerTx {
		end := start + cfg.MaxOpsPerTx
		if end > len(dust) {
			end = len(dust)
		}
		chunk := dust[start:end]

		ops := make([]txnbuild.Operation, 0, len(chunk))
		for _, d := range chunk {
			destMin := d.USDValue.Mul(decimal.NewFromInt(1).Sub(cfg.SlippageTolerance))
			if sameAsset(d.AssetCode, d.Issuer, cfg.TreasuryAssetCode, cfg.TreasuryAssetIssuer) {
				ops = append(ops, &txnbuild.Payment{
					Destination: cfg.TreasuryAccount,
					Amount:      d.Amount.StringFixed(7),
					Asset:       treasuryAsset,
				})
				continue
			}
			ops = append(ops, &txnbuild.PathPaymentStrictSend{
				SendAsset:   creditOrNative(d.AssetCode, d.Issuer),
				SendAmount:  d.Amount.StringFixed(7),
				Destination: cfg.TreasuryAccount,
				DestAsset:   treasuryAsset,
				DestMin:     destMin.StringFixed(7),
			})
		}

		tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
			SourceAccount:        acct,
			IncrementSequenceNum: true,
			Operations:           ops,
			BaseFee:              cfg.BaseFeeStroops,
			Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(30)},
		})
		if err != nil {
			return fmt.Errorf("build dust transaction: %w", err)
		}
		signed, err := s.sign(ctx, tx, w.EncryptedSecret)
		if err != nil {
			return fmt.Errorf("sign dust transaction: %w", err)
		}
		resp, err := stellar.SubmitTransactionWithContext(ctx, s.client(ctx), signed)
		if err != nil {
			return fmt.Errorf("submit dust transaction: %w", err)
		}

		feeXLM := decimal.NewFromInt(cfg.BaseFeeStroops * int64(len(ops))).Div(decimal.NewFromInt(stroopsPerXLM))
		report.TotalFeesConsumed = report.TotalFeesConsumed.Add(feeXLM)
		if xlmUSD, ok := s.usdRate(ctx, "XLM"); ok {
			report.TotalFeesUSD = report.TotalFeesUSD.Add(feeXLM.Mul(xlmUSD))
		}

		collected := decimal.Zero
		for _, d := range chunk {
			collected = collected.Add(d.USDValue)
			report.Sweeps = append(report.Sweeps, DustSweep{
				WalletID:    w.ID,
				AssetCode:   d.AssetCode,
				Issuer:      d.Issuer,
				Amount:      d.Amount,
				Collected:   d.USDValue,
				TxHash:      resp.Hash,
				PathPayment: !sameAsset(d.AssetCode, d.Issuer, cfg.TreasuryAssetCode, cfg.TreasuryAssetIssuer),
			})
			_ = s.repo.UpsertBalance(ctx, w.ID, d.AssetCode, d.Issuer, decimal.Zero)
		}
		report.TotalDustCollected = report.TotalDustCollected.Add(collected)
		if cfg.TreasuryWalletID != "" {
			s.creditCachedBalance(ctx, cfg.TreasuryWalletID, cfg.TreasuryAssetCode, cfg.TreasuryAssetIssuer, collected)
		}
	}
	return nil
}

func (s *service) treasuryAsset(cfg DustConfig) (txnbuild.Asset, error) {
	if strings.EqualFold(cfg.TreasuryAssetCode, "XLM") {
		return txnbuild.NativeAsset{}, nil
	}
	issuer, err := s.resolveIssuer(cfg.TreasuryAssetCode, cfg.TreasuryAssetIssuer)
	if err != nil {
		return nil, err
	}
	return txnbuild.CreditAsset{Code: cfg.TreasuryAssetCode, Issuer: issuer}, nil
}

func creditOrNative(code, issuer string) txnbuild.Asset {
	if strings.EqualFold(code, "XLM") || code == "" {
		return txnbuild.NativeAsset{}
	}
	return txnbuild.CreditAsset{Code: code, Issuer: issuer}
}

func sameAsset(code, issuer, otherCode, otherIssuer string) bool {
	return strings.EqualFold(code, otherCode) && issuer == otherIssuer
}

func (s *service) usdRate(ctx context.Context, code string) (decimal.Decimal, bool) {
	switch strings.ToUpper(code) {
	case "USD", "USDC":
		return decimal.NewFromInt(1), true
	}
	if s.fxSvc == nil {
		return decimal.Zero, false
	}
	rate, err := s.fxSvc.GetRates(ctx, strings.ToUpper(code), "USD")
	if err != nil || rate == nil || rate.Rate.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, false
	}
	return rate.Rate, true
}

func (s *service) valueUSD(ctx context.Context, code string, amount, xlmUSD decimal.Decimal, hasXLMRate bool) (decimal.Decimal, bool) {
	if strings.EqualFold(code, "XLM") {
		if !hasXLMRate {
			return decimal.Zero, false
		}
		return amount.Mul(xlmUSD), true
	}
	rate, ok := s.usdRate(ctx, code)
	if !ok {
		return decimal.Zero, false
	}
	return amount.Mul(rate), true
}

func (s *service) feeUSD(stroops int64, xlmUSD decimal.Decimal, hasXLMRate bool) decimal.Decimal {
	if !hasXLMRate {
		return decimal.Zero
	}
	feeXLM := decimal.NewFromInt(stroops).Div(decimal.NewFromInt(stroopsPerXLM))
	return feeXLM.Mul(xlmUSD)
}

func (s *service) creditCachedBalance(ctx context.Context, walletID, code, issuer string, delta decimal.Decimal) {
	current := decimal.Zero
	if recs, err := s.repo.GetBalances(ctx, walletID); err == nil {
		for _, rec := range recs {
			if strings.EqualFold(rec.AssetCode, code) && rec.Issuer == issuer {
				if parsed, err := decimal.NewFromString(rec.Balance); err == nil {
					current = parsed
				}
				break
			}
		}
	}
	_ = s.repo.UpsertBalance(ctx, walletID, code, issuer, current.Add(delta))
}
