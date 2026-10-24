-- Multi-wallet consolidation with bounded batching (#95).
ALTER TABLE consolidation_operations
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'live',
    ADD COLUMN IF NOT EXISTS merge_sources BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS batch_size INT NOT NULL DEFAULT 20,
    ADD COLUMN IF NOT EXISTS batches JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- SHA-256 of the normalised request, so a reused idempotency key with a
    -- different body is refused instead of replayed.
    ADD COLUMN IF NOT EXISTS request_fingerprint VARCHAR(64) NOT NULL DEFAULT '',
    -- When a worker claimed the operation; a stale claim can be retaken.
    ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMP;

ALTER TABLE consolidation_operations
    ADD CONSTRAINT consolidation_operations_batch_size_check CHECK (batch_size BETWEEN 10 AND 20);

ALTER TABLE consolidation_operations DROP CONSTRAINT IF EXISTS consolidation_operations_status_check;
ALTER TABLE consolidation_operations ADD CONSTRAINT consolidation_operations_status_check
    CHECK (status IN ('pending', 'processing', 'completed', 'partially_completed', 'failed'));
