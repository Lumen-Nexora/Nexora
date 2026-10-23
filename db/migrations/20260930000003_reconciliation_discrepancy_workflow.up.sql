CREATE TABLE reconciliation_discrepancies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    transaction_id UUID NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
    category TEXT NOT NULL CHECK (category IN (
        'missing_onchain_transaction', 'amount_mismatch', 'asset_mismatch',
        'duplicate_settlement', 'stale_pending_state'
    )),
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'acknowledged', 'resolved')),
    last_audit_log_id UUID REFERENCES ledger_audit_log(id) ON DELETE SET NULL,
    assigned_to UUID,
    resolution_note TEXT NOT NULL DEFAULT '',
    resolution_action TEXT CHECK (resolution_action IN ('write_off', 'manual_adjustment', 'partner_recovery')),
    resolution_amount NUMERIC(38, 18),
    approval_status TEXT NOT NULL DEFAULT 'not_required' CHECK (approval_status IN ('not_required', 'pending', 'approved', 'rejected')),
    maker_id UUID,
    checker_id UUID,
    checker_signature TEXT,
    balancing_journal_entry_id UUID,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    UNIQUE (transaction_id, category),
    CHECK (approval_status <> 'approved' OR (checker_id IS NOT NULL AND checker_signature IS NOT NULL)),
    CHECK (checker_id IS NULL OR maker_id IS NULL OR checker_id <> maker_id)
);

CREATE INDEX idx_reconciliation_discrepancies_tenant_status
    ON reconciliation_discrepancies (tenant_id, status, detected_at DESC);
CREATE INDEX idx_reconciliation_discrepancies_category
    ON reconciliation_discrepancies (tenant_id, category, detected_at DESC);
CREATE INDEX idx_reconciliation_discrepancies_approval
    ON reconciliation_discrepancies (tenant_id, approval_status, detected_at DESC);

CREATE TABLE reconciliation_discrepancy_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    discrepancy_id UUID NOT NULL REFERENCES reconciliation_discrepancies(id) ON DELETE CASCADE,
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_id UUID,
    action TEXT NOT NULL CHECK (action IN (
        'acknowledged', 'assigned', 'annotated', 'resolved', 'reopened',
        'resolution_proposed', 'approved', 'rejected', 'journal_posted'
    )),
    note TEXT NOT NULL DEFAULT '',
    assigned_to UUID,
    resolution_action TEXT CHECK (resolution_action IN ('write_off', 'manual_adjustment', 'partner_recovery')),
    resolution_amount NUMERIC(38, 18),
    signature TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_reconciliation_discrepancy_history
    ON reconciliation_discrepancy_history (tenant_id, discrepancy_id, created_at DESC);
