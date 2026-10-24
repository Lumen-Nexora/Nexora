package treasury

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/queue"
	"github.com/Lumen-Nexora/Nexora/internal/tracing"
	"github.com/Lumen-Nexora/Nexora/internal/wallet"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
)

// CustodialSweeper aggregates customer deposits held in custodial wallets
// into tenant master addresses. wallet.SweepEngine satisfies it.
type CustodialSweeper interface {
	SweepAll(ctx context.Context) (wallet.SweepReport, error)
}

// SweepGuard reports whether automated sweeps are halted, e.g. by the
// zero-sum ledger invariant checker. postgres.InvariantRepo satisfies it.
type SweepGuard interface {
	SweepsHalted(ctx context.Context) (halted bool, reason string, err error)
}

type Worker struct {
	svc     Service
	sweeper CustodialSweeper
	guard   SweepGuard
}

func NewWorker(svc Service) *Worker {
	return &Worker{svc: svc}
}

// WithCustodialSweeper enables HandleCustodialSweep. Without one the task is
// a no-op, so a deployment that has not configured sweep thresholds can
// still register the handler safely.
func (w *Worker) WithCustodialSweeper(s CustodialSweeper) *Worker {
	w.sweeper = s
	return w
}

// WithSweepGuard makes both automated sweeps consult guard before moving any
// funds. Without one the sweeps run as before.
func (w *Worker) WithSweepGuard(g SweepGuard) *Worker {
	w.guard = g
	return w
}

// sweepsHalted fails closed: when the halt state cannot be read the sweep is
// skipped and the error returned, because moving funds while the ledger
// might be corrupt is worse than a delayed sweep.
func (w *Worker) sweepsHalted(ctx context.Context, sweep string) (bool, error) {
	if w.guard == nil {
		return false, nil
	}
	logger := tracing.Logger(ctx)
	halted, reason, err := w.guard.SweepsHalted(ctx)
	if err != nil {
		logger.Error().Err(err).Str("sweep", sweep).Msg("sweep halt check failed; skipping sweep")
		return true, err
	}
	if halted {
		logger.Warn().Str("sweep", sweep).Str("reason", reason).Msg("automated sweep halted; skipping run")
	}
	return halted, nil
}

// HandleSweep is registered against the periodic "treasury:sweep" task,
// which asynq's scheduler enqueues once a day (see cmd/worker/main.go). For
// every asset with auto-sweep enabled it sweeps whatever is above the
// configured threshold to cold storage; ExecuteSweep always writes a
// sweep_log row, even a zero-amount one, so every run leaves an audit trail.
// Assets with auto_sweep_enabled = false are skipped entirely — flipping
// that flag off halts the sweeper for that asset on the very next run.
func (w *Worker) HandleSweep(ctx context.Context, task *asynq.Task) error {
	ctx = queue.ContextFromTask(ctx, task)
	ctx, span := tracing.StartConsumer(ctx, task.Type())
	defer span.End()

	if halted, err := w.sweepsHalted(ctx, "treasury"); halted {
		return err
	}

	configs, err := w.svc.GetConfig(ctx)
	if err != nil {
		return err
	}

	// Log through the context so every entry carries the inherited trace_id.
	logger := tracing.Logger(ctx)

	for _, cfg := range configs {
		if !cfg.AutoSweepEnabled {
			continue
		}

		sweepable, err := w.svc.GetSweepableAmount(ctx, cfg.Asset)
		if err != nil {
			logger.Error().Err(err).Str("asset", cfg.Asset).Msg("treasury sweep: failed to compute sweepable amount")
			continue
		}

		amount := decimal.Zero
		if sweepable.GreaterThan(cfg.SweepThreshold) {
			amount = sweepable
		}

		if _, err := w.svc.ExecuteSweep(ctx, cfg.Asset, amount, cfg.ColdStorageAddress, TriggeredByAuto); err != nil {
			logger.Error().Err(err).Str("asset", cfg.Asset).Msg("treasury sweep failed")
		}
	}

	return nil
}

// HandleCustodialSweep is registered against the periodic
// "wallet:custodial_sweep" task (see cmd/worker/main.go). It scans every
// custodial wallet of each tenant that opted in, and sweeps balances above the
// configured thresholds into the tenant master address in batched
// transactions, leaving each wallet its minimum reserve. Individual batch
// failures are recorded and logged by the engine; only a failure to list the
// tenants is returned, so asynq retries the run.
func (w *Worker) HandleCustodialSweep(ctx context.Context, task *asynq.Task) error {
	if w.sweeper == nil {
		return nil
	}
	ctx = queue.ContextFromTask(ctx, task)
	ctx, span := tracing.StartConsumer(ctx, task.Type())
	defer span.End()

	if halted, err := w.sweepsHalted(ctx, "custodial"); halted {
		return err
	}

	report, err := w.sweeper.SweepAll(ctx)
	logger := tracing.Logger(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("custodial sweep run failed")
		return err
	}
	logger.Info().
		Int("targets", report.Targets).
		Int("wallets_scanned", report.WalletsScanned).
		Int("wallets_swept", report.WalletsSwept).
		Int("transactions", report.Transactions).
		Int("failed_transactions", report.FailedTransactions).
		Interface("skipped", report.Skipped).
		Msg("custodial sweep run completed")
	return nil
}
