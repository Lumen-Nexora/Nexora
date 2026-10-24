-- Auto-remediation of transient reconciliation discrepancies (#120).
--
-- A confirmed transaction that Horizon has not ingested yet is usually just
-- Horizon lagging the database by a ledger or two. Instead of flagging it on
-- the spot, the reconciler records an observation here and re-checks it on
-- each subsequent ledger close. It is resolved only when Horizon shows the
-- transaction and every reconciliation check passes; otherwise it is escalated
-- to an operator after the configured number of consecutive cycles.
CREATE TABLE IF NOT EXISTS reconciliation_transient_observations (
    id                UUID PRIMARY KEY,
    transaction_id    UUID NOT NULL UNIQUE REFERENCES transactions(id),
    tenant_id         UUID,
    mode              TEXT NOT NULL DEFAULT 'live',
    tx_hash           TEXT NOT NULL,
    category          TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'observing'
                      CHECK (status IN ('observing', 'resolved', 'escalated')),
    cycles            INTEGER NOT NULL DEFAULT 1 CHECK (cycles >= 1),
    first_ledger      BIGINT NOT NULL DEFAULT 0,
    last_ledger       BIGINT NOT NULL DEFAULT 0,
    detected_at       TIMESTAMPTZ NOT NULL,
    last_evaluated_at TIMESTAMPTZ NOT NULL,
    resolved_at       TIMESTAMPTZ,
    reason            TEXT NOT NULL DEFAULT '',
    -- Head of this observation's SHA-256 event chain. Doubles as the
    -- compare-and-swap token for state transitions.
    trace_hash        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_transient_observations_due
    ON reconciliation_transient_observations (last_evaluated_at)
    WHERE status = 'observing';

-- Every remediation action, chained by hash = sha256(prev_hash | event).
CREATE TABLE IF NOT EXISTS reconciliation_transient_events (
    id             UUID PRIMARY KEY,
    observation_id UUID NOT NULL REFERENCES reconciliation_transient_observations(id),
    transaction_id UUID NOT NULL,
    tx_hash        TEXT NOT NULL,
    action         TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    cycle          INTEGER NOT NULL,
    ledger         BIGINT NOT NULL DEFAULT 0,
    prev_hash      TEXT NOT NULL,
    hash           TEXT NOT NULL UNIQUE,
    created_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_transient_events_observation
    ON reconciliation_transient_events (observation_id, created_at);

-- The event chain is an audit trail: rows can never be changed or removed.
CREATE OR REPLACE FUNCTION reconciliation_transient_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation_transient_events is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_reconciliation_transient_events_append_only ON reconciliation_transient_events;
CREATE TRIGGER trg_reconciliation_transient_events_append_only
    BEFORE UPDATE OR DELETE ON reconciliation_transient_events
    FOR EACH ROW EXECUTE FUNCTION reconciliation_transient_events_append_only();
