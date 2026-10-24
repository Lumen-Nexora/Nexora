ALTER TABLE tenants
    ADD COLUMN risk_category VARCHAR(16) NOT NULL DEFAULT 'medium'
    CHECK (risk_category IN ('low', 'medium', 'high'));
