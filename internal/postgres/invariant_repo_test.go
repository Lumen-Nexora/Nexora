package postgres_test

import (
	"context"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/postgres"
	"github.com/Lumen-Nexora/Nexora/internal/reconcile"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/keypair"
)

// TestInvariantRepoAgainstMigratedSchema runs the real invariant SQL against a
// freshly migrated database with several tenants, a test-mode tenant and
// pooled reserve wallets, then corrupts one balance.
func TestInvariantRepoAgainstMigratedSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping postgres invariant test in short mode")
	}
	dbURL := startPostgres(t)
	if err := postgres.RunMigrations(dbURL, "../../db/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := postgres.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	repo := postgres.NewInvariantRepo(pool)

	// An empty, unconfigured ledger is skipped rather than evaluated.
	if _, configured, err := repo.GetInvariantTotals(ctx); err != nil || configured {
		t.Fatalf("empty ledger: configured=%v err=%v", configured, err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	tenant := func() string {
		id := uuid.NewString()
		exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'T', $2)`, id, id+"@example.com")
		return id
	}
	wallet := func(tenantID *string, mode string) string {
		id := uuid.NewString()
		exec(`INSERT INTO wallets (id, public_key, encrypted_secret, tenant_id, mode) VALUES ($1, $2, 'x', $3, $4)`,
			id, keypair.MustRandom().Address(), tenantID, mode)
		return id
	}
	balance := func(walletID, asset, amount string) {
		exec(`INSERT INTO balances (wallet_id, asset_code, issuer, balance) VALUES ($1, $2, '', $3)
		      ON CONFLICT (wallet_id, asset_code, issuer) DO UPDATE SET balance = EXCLUDED.balance`,
			walletID, asset, amount)
	}
	fee := func(tenantID, mode, asset, amount string) {
		txID := uuid.NewString()
		exec(`INSERT INTO transactions (id, type, asset, amount, mode) VALUES ($1, 'transfer', $2, 1, $3)`, txID, asset, mode)
		exec(`INSERT INTO fee_collections (transaction_id, tenant_id, fee_amount, asset, fee_bps) VALUES ($1, $2, $3, $4, 30)`,
			txID, tenantID, amount, asset)
	}

	tenantA, tenantB, tenantTest := tenant(), tenant(), tenant()
	a1, a2 := wallet(&tenantA, "live"), wallet(&tenantA, "live")
	b1 := wallet(&tenantB, "live")
	testWallet := wallet(&tenantTest, "test")
	reserve := wallet(nil, "live")
	feeWallet := wallet(nil, "live")
	exec(`INSERT INTO pooled_reserve_wallets (wallet_id) VALUES ($1), ($2)`, reserve, feeWallet)

	balance(a1, "XLM", "100.0000001")
	balance(a2, "XLM", "50.0000000")
	balance(b1, "XLM", "999999999999.0000000")
	balance(b1, "USDC", "25.5000000")
	balance(testWallet, "XLM", "777.0000000") // test mode: excluded everywhere
	fee(tenantA, "live", "XLM", "1.5000000")
	fee(tenantB, "live", "USDC", "0.2500000")
	fee(tenantTest, "test", "XLM", "3.0000000") // test mode: excluded
	// 0.5 XLM of fees already swept to cold storage left the pool.
	exec(`INSERT INTO sweep_log (asset, amount, triggered_by) VALUES ('XLM', 0.5, 'auto')`)
	// Pool: tenants 1000000000149.0000001 + net fees 1.0 for XLM.
	balance(reserve, "XLM", "1000000000149.0000001")
	balance(feeWallet, "XLM", "1.0000000")
	balance(reserve, "USDC", "25.5000000")
	balance(feeWallet, "USDC", "0.2500000")

	totals, configured, err := repo.GetInvariantTotals(ctx)
	if err != nil || !configured {
		t.Fatalf("GetInvariantTotals: configured=%v err=%v", configured, err)
	}
	results := reconcile.EvaluateInvariant(totals)
	if len(results) != 2 {
		t.Fatalf("expected USDC and XLM, got %+v", results)
	}
	for _, r := range results {
		if !r.Balanced {
			t.Fatalf("expected balanced books, got %+v", r)
		}
	}
	xlm := results[1]
	if xlm.Asset != "XLM" || !xlm.TenantBalances.Equal(decimal.RequireFromString("1000000000149.0000001")) ||
		!xlm.AccumulatedFees.Equal(decimal.RequireFromString("1")) {
		t.Fatalf("unexpected XLM totals: %+v", xlm)
	}

	// Simulated corruption: tenant B's USDC balance inflated by one stroop.
	balance(b1, "USDC", "25.5000001")
	totals, _, err = repo.GetInvariantTotals(ctx)
	if err != nil {
		t.Fatalf("GetInvariantTotals: %v", err)
	}
	results = reconcile.EvaluateInvariant(totals)
	if results[0].Asset != "USDC" || results[0].Balanced ||
		!results[0].Difference.Equal(decimal.RequireFromString("-0.0000001")) {
		t.Fatalf("expected a one-stroop USDC violation, got %+v", results[0])
	}
	if !results[1].Balanced {
		t.Fatalf("XLM must stay balanced, got %+v", results[1])
	}

	// Halts are idempotent and scoped to their source.
	if halted, _, err := repo.SweepsHalted(ctx); err != nil || halted {
		t.Fatalf("expected no halt, got halted=%v err=%v", halted, err)
	}
	for i, want := range []bool{true, false} {
		newly, err := repo.HaltSweeps(ctx, reconcile.InvariantHaltSource, "USDC imbalance")
		if err != nil || newly != want {
			t.Fatalf("HaltSweeps #%d: newly=%v err=%v, want %v", i+1, newly, err, want)
		}
	}
	exec(`INSERT INTO sweep_halts (halted_by, reason) VALUES ('operator', 'manual')`)
	if halted, reason, err := repo.SweepsHalted(ctx); err != nil || !halted || reason == "" {
		t.Fatalf("expected a halt, got halted=%v reason=%q err=%v", halted, reason, err)
	}
	if resumed, err := repo.ResumeSweeps(ctx, reconcile.InvariantHaltSource); err != nil || !resumed {
		t.Fatalf("ResumeSweeps: resumed=%v err=%v", resumed, err)
	}
	if halted, reason, err := repo.SweepsHalted(ctx); err != nil || !halted || reason != "operator: manual" {
		t.Fatalf("operator halt must survive, got halted=%v reason=%q err=%v", halted, reason, err)
	}
}
