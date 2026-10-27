DROP TRIGGER IF EXISTS reconciliation_drift_snapshot_immutable ON reconciliation_drift_snapshots;
DROP FUNCTION IF EXISTS prevent_hourly_drift_snapshot_mutation();
DROP INDEX IF EXISTS idx_reconciliation_drift_hourly_asset;

DELETE FROM reconciliation_drift_snapshots WHERE snapshot_hour IS NOT NULL;

ALTER TABLE reconciliation_drift_snapshots
    DROP COLUMN IF EXISTS snapshot_hour,
    ALTER COLUMN expected_balance TYPE NUMERIC(20, 7),
    ALTER COLUMN actual_balance TYPE NUMERIC(20, 7),
    ALTER COLUMN drift_amount TYPE NUMERIC(20, 7),
    ALTER COLUMN wallet_id SET NOT NULL,
    ALTER COLUMN wallet_address SET NOT NULL;
