DROP INDEX IF EXISTS idx_custodial_sweeps_items;
DROP INDEX IF EXISTS idx_custodial_sweeps_tenant_created;
DROP TABLE IF EXISTS custodial_sweeps;
ALTER TABLE tenant_custody_settings DROP COLUMN IF EXISTS auto_sweep_enabled;
