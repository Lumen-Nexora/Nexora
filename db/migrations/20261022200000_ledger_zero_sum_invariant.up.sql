-- Zero-sum ledger invariant checker (#117).

-- Wallets that hold the platform's pooled reserves. The checker asserts
--   SUM(tenant balances) + accumulated fees == SUM(pooled reserve balances)
-- per asset, so the reserve side has to be declared explicitly rather than
-- guessed. While this table is empty the checker reports "unconfigured" and
-- never halts anything. The platform fee wallet belongs here too: collected
-- fees sit in it until a treasury sweep moves them to cold storage.
CREATE TABLE IF NOT EXISTS pooled_reserve_wallets (
    wallet_id  UUID PRIMARY KEY REFERENCES wallets(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one row per halting component. While any row exists, the automated
-- treasury and custodial sweeps are skipped. The invariant checker owns the
-- 'ledger_invariant' row: it inserts it on a violation and deletes it once
-- the books balance again. Rows from other sources are never touched by it.
CREATE TABLE IF NOT EXISTS sweep_halts (
    halted_by  TEXT PRIMARY KEY,
    reason     TEXT NOT NULL,
    halted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
