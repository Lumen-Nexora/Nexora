package wallet

import (
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/assets"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/stellar/go/txnbuild"
)

// sacResolver derives the Stellar Asset Contract address for a classic asset.
// Soroban contracts move classic assets by calling the SAC, whose address is
// deterministic from the asset and the network passphrase.
type sacResolver struct {
	registry   *assets.Registry
	passphrase string
	usdcIssuer string
	eurcIssuer string
}

// NewSACResolver builds the AssetResolver used by the contract wallet adapter.
func NewSACResolver(stellarNetwork, usdcIssuer, eurcIssuer string) AssetResolver {
	return &sacResolver{
		registry:   assets.NewRegistry(usdcIssuer, eurcIssuer),
		passphrase: stellar.NetworkPassphraseFor(stellarNetwork),
		usdcIssuer: usdcIssuer,
		eurcIssuer: eurcIssuer,
	}
}

func (r *sacResolver) ContractAddress(assetCode, issuer string) (string, error) {
	var asset txnbuild.Asset = txnbuild.NativeAsset{}

	if assetCode != "" && assetCode != "XLM" {
		if issuer == "" {
			if a, ok := r.registry.Get(assetCode); ok && a.Issuer != "" {
				issuer = a.Issuer
			} else if assetCode == "USDC" {
				issuer = r.usdcIssuer
			} else if assetCode == "EURC" {
				issuer = r.eurcIssuer
			}
		}
		if issuer == "" {
			return "", fmt.Errorf("%w: missing issuer for asset %s", domain.ErrInvalidAsset, assetCode)
		}
		asset = txnbuild.CreditAsset{Code: assetCode, Issuer: issuer}
	}

	xdrAsset, err := asset.ToXDR()
	if err != nil {
		return "", fmt.Errorf("encode asset %s: %w", assetCode, err)
	}

	contractID, err := xdrAsset.ContractID(r.passphrase)
	if err != nil {
		return "", fmt.Errorf("derive asset contract id for %s: %w", assetCode, err)
	}

	return stellar.EncodeContractID(contractID)
}
