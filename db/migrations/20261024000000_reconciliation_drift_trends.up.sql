ALTER TABLE reconciliation_drift_snapshots
    ALTER COLUMN wallet_id DROP NOT NULL,
    ALTER COLUMN wallet_address DROP NOT NULL,
    ALTER COLUMN expected_balance TYPE NUMERIC(38, 7),
    ALTER COLUMN actual_balance TYPE NUMERIC(38, 7),
    ALTER COLUMN drift_amount TYPE NUMERIC(38, 7),
    ADD COLUMN snapshot_hour TIMESTAMPTZ;

CREATE UNIQUE INDEX idx_reconciliation_drift_hourly_asset
    ON reconciliation_drift_snapshots (asset, snapshot_hour)
    WHERE snapshot_hour IS NOT NULL;

CREATE FUNCTION prevent_hourly_drift_snapshot_mutation() RETURNS trigger AS $$
BEGIN
    IF OLD.snapshot_hour IS NOT NULL OR (TG_OP = 'UPDATE' AND NEW.snapshot_hour IS NOT NULL) THEN
        RAISE EXCEPTION 'hourly reconciliation drift snapshots are immutable';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER reconciliation_drift_snapshot_immutable
    BEFORE UPDATE OR DELETE ON reconciliation_drift_snapshots
    FOR EACH ROW EXECUTE FUNCTION prevent_hourly_drift_snapshot_mutation();
