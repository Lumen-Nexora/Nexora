package postgres

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// ClaimableWalletResolver adapts WalletRepo to the narrow wallet view the
// claimable service needs. The adapter exists so internal/claimable does not
// depend on internal/wallet: claimable only ever needs a public key and a
// secret to sign with, not the wallet service.
//
// Both lookups stay tenant-scoped through WalletRepo, because the tenant is on
// the context — an org can therefore only fund or claim with its own wallets.
type ClaimableWalletResolver struct {
	repo *WalletRepo
}

func NewClaimableWalletResolver(repo *WalletRepo) *ClaimableWalletResolver {
	return &ClaimableWalletResolver{repo: repo}
}

func (r *ClaimableWalletResolver) GetByID(ctx context.Context, walletID string) (*domain.SourceWallet, error) {
	wallet, err := r.repo.GetByID(ctx, walletID)
	if err != nil {
		return nil, err
	}
	if source := toSourceWallet(wallet); source != nil {
		return source, nil
	}
	return nil, domain.ErrWalletNotFound
}

func (r *ClaimableWalletResolver) GetByPublicKey(ctx context.Context, publicKey string) (*domain.SourceWallet, error) {
	wallet, err := r.repo.GetByPublicKey(ctx, publicKey)
	if err != nil {
		return nil, err
	}
	if source := toSourceWallet(wallet); source != nil {
		return source, nil
	}
	return nil, domain.ErrWalletNotFound
}

// toSourceWallet returns nil for a wallet Nexora cannot sign for. Contract
// wallets have no custodial secret, so they can neither fund a claimable
// balance nor claim one on a claimant's behalf.
func toSourceWallet(wallet *domain.Wallet) *domain.SourceWallet {
	if wallet == nil || wallet.EncryptedSecret == "" {
		return nil
	}
	return &domain.SourceWallet{
		ID:              wallet.ID,
		PublicKey:       wallet.PublicKey,
		EncryptedSecret: wallet.EncryptedSecret,
	}
}
