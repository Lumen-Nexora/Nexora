-- Automated custodial reserve sweep engine (#94).

-- Sweeping moves customer deposits, so it is opt-in per tenant and mode. The
-- destination is the tenant master address already held here (#96).
ALTER TABLE tenant_custody_settings
    ADD COLUMN IF NOT EXISTS auto_sweep_enabled BOOLEAN NOT NULL DEFAULT FALSE;

-- One row per submitted sweep transaction, itemised per wallet and asset.
CREATE TABLE IF NOT EXISTS custodial_sweeps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    mode TEXT NOT NULL DEFAULT 'live',
    destination_address VARCHAR(56) NOT NULL,
    transaction_hash VARCHAR(64),
    status VARCHAR(20) NOT NULL CHECK (status IN ('completed', 'failed')),
    fee_stroops BIGINT NOT NULL DEFAULT 0,
    items JSONB NOT NULL DEFAULT '[]'::jsonb,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_custodial_sweeps_tenant_created
    ON custodial_sweeps(tenant_id, mode, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_custodial_sweeps_items
    ON custodial_sweeps USING GIN (items jsonb_path_ops);
