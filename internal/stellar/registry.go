package stellar

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
)

// ClientResolver selects an immutable Horizon client for the authenticated
// environment. Keeping this separate from Client preserves small fakes in
// existing service tests while making mode selection explicit in production.
type ClientResolver interface {
	ClientForMode(ctx context.Context) Client
}

type modeClientResolver struct {
	live Client
	test Client
}

func NewModeAwareClients(live, test Client) ClientResolver {
	return &modeClientResolver{live: live, test: test}
}

func (r *modeClientResolver) ClientForMode(ctx context.Context) Client {
	if mode, ok := tenant.ModeFromContext(ctx); ok && mode == domain.ModeTest && r.test != nil {
		return r.test
	}
	return r.live
}

// SignerResolver selects the network passphrase/key material for a mode.
type SignerResolver interface {
	SignerForMode(ctx context.Context) Signer
}

type modeSignerResolver struct {
	live Signer
	test Signer
}

func NewModeAwareSigners(live, test Signer) SignerResolver {
	return &modeSignerResolver{live: live, test: test}
}

func (r *modeSignerResolver) SignerForMode(ctx context.Context) Signer {
	if mode, ok := tenant.ModeFromContext(ctx); ok && mode == domain.ModeTest && r.test != nil {
		return r.test
	}
	return r.live
}
