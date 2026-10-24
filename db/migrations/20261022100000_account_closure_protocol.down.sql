DROP TRIGGER IF EXISTS trg_account_closures_append_only ON account_closures;
-- destination_wallet stays nullable: closures swept to an external master
-- address have no wallet row to point at.
DROP FUNCTION IF EXISTS account_closures_append_only();
DROP INDEX IF EXISTS idx_account_closures_one_live;
ALTER TABLE account_closures DROP COLUMN IF EXISTS native_balance;
ALTER TABLE account_closures DROP COLUMN IF EXISTS removed_trustlines;
ALTER TABLE account_closures DROP COLUMN IF EXISTS justification;
ALTER TABLE account_closures DROP COLUMN IF EXISTS destination_address;
ALTER TABLE account_closures DROP COLUMN IF EXISTS mode;
DROP TABLE IF EXISTS tenant_custody_settings;
