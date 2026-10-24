DROP INDEX IF EXISTS idx_tenant_audit_events_chain;
ALTER TABLE tenant_audit_events DROP COLUMN IF EXISTS prev_row_hash;
ALTER TABLE tenant_audit_events DROP COLUMN IF EXISTS row_hash;
