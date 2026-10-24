CREATE TABLE travel_rule_exchanges (
    message_id UUID PRIMARY KEY,
    transaction_id UUID NOT NULL UNIQUE REFERENCES transactions(id) ON DELETE CASCADE,
    tenant_id UUID,
    mode TEXT NOT NULL,
    recipient_vasp_id TEXT NOT NULL,
    protocol VARCHAR(20) NOT NULL CHECK (protocol IN ('trp', 'openvasp')),
    envelope_sha256 CHAR(64) NOT NULL,
    encrypted_envelope JSONB NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'rejected')),
    release_status transaction_status NOT NULL,
    confirmation_id TEXT UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ
);

CREATE INDEX idx_travel_rule_exchanges_status_created
    ON travel_rule_exchanges(status, created_at);
