package treasury

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// Config holds the per-asset sweep policy stored in treasury_config.
type Config = domain.TreasuryConfig

// SweepLog is one row of the sweep audit trail, written on every sweep
// attempt — including "zero sweeps" where nothing was moved.
type SweepLog = domain.TreasurySweepLog

const (
	TriggeredByAuto   = domain.TriggeredByAuto
	TriggeredByManual = domain.TriggeredByManual
)

// Repository persists treasury sweep configuration and the sweep audit log.
type Repository interface {
	GetConfig(ctx context.Context, asset string) (*domain.TreasuryConfig, error)
	ListConfig(ctx context.Context) ([]*domain.TreasuryConfig, error)
	UpdateConfig(ctx context.Context, cfg *domain.TreasuryConfig) error
	// ListWalletPublicKeys returns the Stellar public key of every wallet
	// Nexora custodies, across all tenants — treasury reserve accounting is a
	// platform-wide concern, not scoped to a single org.
	ListWalletPublicKeys(ctx context.Context) ([]string, error)
	RecordSweep(ctx context.Context, log *domain.TreasurySweepLog) error
	ListSweeps(ctx context.Context, limit, offset int) ([]*domain.TreasurySweepLog, error)
}
