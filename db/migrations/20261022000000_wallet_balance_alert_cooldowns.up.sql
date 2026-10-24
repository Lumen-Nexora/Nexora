-- Wallet Balance Alert Cooldowns table for debouncing
-- This table tracks the last time an alert was triggered for a specific wallet/asset combination
-- to prevent notification spam within a cooldown window
CREATE TABLE IF NOT EXISTS wallet_balance_alert_cooldowns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id UUID NOT NULL REFERENCES wallet_balance_alerts(id) ON DELETE CASCADE,
    wallet_id UUID NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
    asset_code VARCHAR(12) NOT NULL,
    asset_issuer VARCHAR(56),
    last_triggered_at TIMESTAMP NOT NULL,
    cooldown_window_seconds INTEGER NOT NULL DEFAULT 3600,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (alert_id, wallet_id, asset_code, COALESCE(asset_issuer, ''))
);

CREATE INDEX idx_wallet_balance_alert_cooldowns_alert ON wallet_balance_alert_cooldowns(alert_id);
CREATE INDEX idx_wallet_balance_alert_cooldowns_wallet ON wallet_balance_alert_cooldowns(wallet_id);
CREATE INDEX idx_wallet_balance_alert_cooldowns_last_triggered ON wallet_balance_alert_cooldowns(last_triggered_at);
