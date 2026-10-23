-- Inbound memo validation is configured per custodial wallet: a shared
-- deposit account can require a memo in a given format and, when the memo
-- does not match, the deposit is queued for investigation instead of being
-- credited to a customer.
CREATE TABLE wallet_memo_policies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    wallet_id     UUID NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
    format        TEXT NOT NULL
        CHECK (format IN ('customer_id', 'email', 'phone', 'reference', 'custom')),
    pattern       TEXT NOT NULL DEFAULT '',
    require_memo  BOOLEAN NOT NULL DEFAULT TRUE,
    auto_refund   BOOLEAN NOT NULL DEFAULT FALSE,
    refund_after  INTERVAL NOT NULL DEFAULT INTERVAL '24 hours'
        CHECK (refund_after > INTERVAL '0' AND refund_after <= INTERVAL '30 days'),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, wallet_id)
);

CREATE INDEX idx_wallet_memo_policies_tenant ON wallet_memo_policies (tenant_id);
CREATE INDEX idx_wallet_memo_policies_wallet ON wallet_memo_policies (wallet_id);

-- The investigation queue. A deposit that arrived on a wallet with a memo
-- policy but could not be mapped is stored here instead of being credited.
-- The row is both the queue entry and the durable audit trail of the
-- deposit.
CREATE TABLE unmapped_deposits (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    wallet_id       UUID NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
    mode            TEXT NOT NULL DEFAULT 'live' CHECK (mode IN ('live', 'test')),
    tx_hash         TEXT NOT NULL,
    source_address  TEXT NOT NULL,
    asset           TEXT NOT NULL,
    issuer          TEXT NOT NULL DEFAULT '',
    amount          NUMERIC(30, 7) NOT NULL,
    memo            TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL CHECK (reason IN ('missing_memo', 'invalid_memo')),
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'refunding', 'refunded', 'resolved')),
    refund_after    TIMESTAMPTZ NOT NULL,
    refund_tx_hash  TEXT NOT NULL DEFAULT '',
    resolution      TEXT NOT NULL DEFAULT '',
    failure_reason  TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    -- One queue entry per deposit. The indexer replays payments, so the same
    -- payment must not be filed twice.
    UNIQUE (mode, wallet_id, tx_hash)
);

CREATE INDEX idx_unmapped_deposits_tenant_status
    ON unmapped_deposits (tenant_id, status, created_at DESC);
CREATE INDEX idx_unmapped_deposits_wallet
    ON unmapped_deposits (wallet_id, status);
-- The refund sweep only looks at pending deposits whose timeout expired.
CREATE INDEX idx_unmapped_deposits_refund_due
    ON unmapped_deposits (refund_after) WHERE status = 'pending';
