-- Hash-chain columns for the tamper-proof custodial reserve audit trail.
-- Every audit event stores the hash of the row that preceded it and its own
-- row hash, so retroactive row modifications, insertions, or deletions break
-- a detectable chain of SHA-256 digests.
-- Legacy rows written before this migration keep NULL and are excluded from
-- the chain until the first chained row links the chain forward from there.
ALTER TABLE tenant_audit_events ADD COLUMN IF NOT EXISTS prev_row_hash BYTEA;
ALTER TABLE tenant_audit_events ADD COLUMN IF NOT EXISTS row_hash BYTEA;

-- The verifier and the daily bundle export scan the chain in append order.
CREATE INDEX IF NOT EXISTS idx_tenant_audit_events_chain
    ON tenant_audit_events(created_at, id)
    WHERE row_hash IS NOT NULL;
