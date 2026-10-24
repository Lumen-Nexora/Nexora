UPDATE consolidation_operations SET status = 'failed' WHERE status IN ('processing', 'partially_completed');
ALTER TABLE consolidation_operations DROP CONSTRAINT IF EXISTS consolidation_operations_status_check;
ALTER TABLE consolidation_operations ADD CONSTRAINT consolidation_operations_status_check
    CHECK (status IN ('pending', 'completed', 'failed'));
ALTER TABLE consolidation_operations DROP CONSTRAINT IF EXISTS consolidation_operations_batch_size_check;
ALTER TABLE consolidation_operations
    DROP COLUMN IF EXISTS claimed_at,
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS batches,
    DROP COLUMN IF EXISTS batch_size,
    DROP COLUMN IF EXISTS merge_sources,
    DROP COLUMN IF EXISTS mode;
