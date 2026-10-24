-- Exact on-chain network fee accounting (issue #118).
CREATE TABLE IF NOT EXISTS network_fee_charges (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id      UUID REFERENCES transactions(id) ON DELETE SET NULL,
    tx_hash             TEXT NOT NULL,
    tenant_id           UUID REFERENCES tenants(id) ON DELETE SET NULL,
    sub_account         TEXT,
    cost_center         TEXT NOT NULL DEFAULT 'blockchain_network',
    allocation          TEXT NOT NULL CHECK (allocation IN ('tenant', 'platform_overhead')),
    ledger_sequence     INT NOT NULL CHECK (ledger_sequence >= 0),
    ledger_closed_at    TIMESTAMPTZ NOT NULL,
    fee_charged_stroops BIGINT NOT NULL CHECK (fee_charged_stroops >= 0),
    xlm_amount          NUMERIC(20, 7) NOT NULL,
    xlm_usd_rate        NUMERIC(38, 18),
    usd_amount          NUMERIC(28, 10),
    valued              BOOLEAN NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_network_fee_valuation CHECK (
        (valued = FALSE AND xlm_usd_rate IS NULL AND usd_amount IS NULL)
        OR (valued = TRUE AND xlm_usd_rate IS NOT NULL AND usd_amount IS NOT NULL)
    )
);

-- One charge per on-chain transaction: settlement retries must never allocate
-- the same network fee twice.
CREATE UNIQUE INDEX IF NOT EXISTS uq_network_fee_charges_tx_hash
    ON network_fee_charges (tx_hash);

CREATE INDEX IF NOT EXISTS idx_network_fee_charges_tenant_time
    ON network_fee_charges (tenant_id, ledger_closed_at DESC);

CREATE INDEX IF NOT EXISTS idx_network_fee_charges_time
    ON network_fee_charges (ledger_closed_at DESC);
