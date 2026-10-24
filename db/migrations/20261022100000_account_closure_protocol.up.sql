-- Destructive account closure protocol (#96).

-- Per-tenant, per-mode custody settings. master_address receives the native
-- XLM swept by AccountMerge; primary_settlement_wallet_id can never be closed.
CREATE TABLE IF NOT EXISTS tenant_custody_settings (
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    mode TEXT NOT NULL DEFAULT 'live',
    master_address VARCHAR(56) NOT NULL CHECK (master_address ~ '^G[A-Z2-7]{55}$'),
    primary_settlement_wallet_id UUID REFERENCES wallets(id),
    updated_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, mode)
);

-- Closures now sweep to the tenant master address, which need not be a
-- Nexora-managed wallet, and carry the operator's justification.
ALTER TABLE account_closures ALTER COLUMN destination_wallet DROP NOT NULL;
ALTER TABLE account_closures ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'live';
ALTER TABLE account_closures ADD COLUMN IF NOT EXISTS destination_address VARCHAR(56);
ALTER TABLE account_closures ADD COLUMN IF NOT EXISTS justification TEXT NOT NULL DEFAULT '';
ALTER TABLE account_closures ADD COLUMN IF NOT EXISTS removed_trustlines JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE account_closures ADD COLUMN IF NOT EXISTS native_balance DECIMAL(38, 7);

-- At most one closure may be in flight or completed per wallet, so two
-- operators racing on the same wallet cannot both submit an AccountMerge.
CREATE UNIQUE INDEX IF NOT EXISTS idx_account_closures_one_live
    ON account_closures(wallet_id) WHERE status IN ('pending', 'completed');

-- Closure records are a permanent compliance trail: rows cannot be deleted,
-- who/why/where fields never change, and a finished closure is frozen.
CREATE OR REPLACE FUNCTION account_closures_append_only() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'account_closures is append-only';
    END IF;
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'account closure % is final', OLD.id;
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id
        OR NEW.wallet_id <> OLD.wallet_id
        OR NEW.mode <> OLD.mode
        OR NEW.actor_type <> OLD.actor_type
        OR NEW.actor_id <> OLD.actor_id
        OR NEW.justification <> OLD.justification
        OR NEW.destination_address IS DISTINCT FROM OLD.destination_address
        OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'account closure % identity fields are immutable', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_account_closures_append_only ON account_closures;
CREATE TRIGGER trg_account_closures_append_only
    BEFORE UPDATE OR DELETE ON account_closures
    FOR EACH ROW EXECUTE FUNCTION account_closures_append_only();
