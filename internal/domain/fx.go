package domain

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Quote is a priced, time-limited conversion offer identified by a unique token.
type Quote struct {
	ID                    string
	OrgID                 string
	FromAsset             string
	ToAsset               string
	FromAmount            decimal.Decimal
	ToAmount              decimal.Decimal
	Rate                  decimal.Decimal
	Fee                   decimal.Decimal
	ExpiresAt             time.Time
	Used                  bool
	FromRequiresTrustline bool
	ToRequiresTrustline   bool
}

// AlertDirection specifies whether the alert fires when the rate crosses above or below the target.
type AlertDirection string

const (
	AlertDirectionAtOrAbove AlertDirection = "at_or_above"
	AlertDirectionAtOrBelow AlertDirection = "at_or_below"
)

// RateAlert represents a tenant-configured threshold notification for an FX pair.
type RateAlert struct {
	ID           string           `json:"id"`
	TenantID     string           `json:"tenant_id"`
	FromAsset    string           `json:"from_asset"`
	ToAsset      string           `json:"to_asset"`
	TargetRate   decimal.Decimal  `json:"target_rate"`
	Direction    AlertDirection   `json:"direction"`
	ExpiresAt    *time.Time       `json:"expires_at,omitempty"`
	LastFiredAt  *time.Time       `json:"last_fired_at,omitempty"`
	LastEvalRate *decimal.Decimal `json:"last_eval_rate,omitempty"`
	Active       bool             `json:"active"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// RateLockStatus represents the lifecycle state of a rate lock.
type RateLockStatus string

const (
	RateLockStatusActive   RateLockStatus = "active"
	RateLockStatusConsumed RateLockStatus = "consumed"
	RateLockStatusExpired  RateLockStatus = "expired"
)

// RateLock holds a quoted rate for a bounded window, consumed by exactly one conversion.
type RateLock struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	FromAsset  string          `json:"from_asset"`
	ToAsset    string          `json:"to_asset"`
	LockedRate decimal.Decimal `json:"locked_rate"`
	Amount     decimal.Decimal `json:"amount"`
	ExpiresAt  time.Time       `json:"expires_at"`
	ConsumedAt *time.Time      `json:"consumed_at,omitempty"`
	ConsumedBy *string         `json:"consumed_by,omitempty"` // conversion ID
	Status     RateLockStatus  `json:"status"`
	WalletID   *string         `json:"wallet_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// TenantFXMargin represents a tenant-specific custom FX markup/spread margin rule.
type TenantFXMargin struct {
	ID            string           `json:"id"`
	TenantID      string           `json:"tenant_id"`
	FromAsset     string           `json:"from_asset"`
	ToAsset       string           `json:"to_asset"`
	TierMinAmount decimal.Decimal  `json:"tier_min_amount"`
	TierMaxAmount *decimal.Decimal `json:"tier_max_amount,omitempty"`
	MarkupBps     int              `json:"markup_bps"`
	MinMarginBps  int              `json:"min_margin_bps"`
	MaxMarginBps  int              `json:"max_margin_bps"`
	CreatedBy     string           `json:"created_by"`
	UpdatedBy     string           `json:"updated_by"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// Platform-allowable tenant FX margin ceilings (basis points)
// -500 bps = -5.00% max allowable discount, 5000 bps = +50.00% max allowable
// markup.
const (
	MinAllowableMarginBps = -500
	MaxAllowableMarginBps = 5000
)

// TenantFXRepo handles persistence, retrieval, tier resolution, and audit
// tracking for multi-tenant custom FX markups. It is declared here — next to
// the aggregates it manages — so postgres storage and the fx service can both
// depend on it without importing each other.
type TenantFXRepo interface {
	CreateMargin(ctx context.Context, m *TenantFXMargin, actorPrincipal, actorIP string) error
	GetMargin(ctx context.Context, id, tenantID string) (*TenantFXMargin, error)
	FindMargin(ctx context.Context, tenantID, fromAsset, toAsset string, amount decimal.Decimal) (*TenantFXMargin, error)
	ListMargins(ctx context.Context, tenantID, fromAsset, toAsset string, limit, offset int) ([]*TenantFXMargin, int, error)
	UpdateMargin(ctx context.Context, m *TenantFXMargin, actorPrincipal, actorIP string) error
	DeleteMargin(ctx context.Context, id, tenantID, actorPrincipal, actorIP string) error
	ListMarginAudits(ctx context.Context, tenantID, marginID string, limit, offset int) ([]*TenantFXMarginAudit, error)
}

// TenantFXMarginAudit records historical modifications to tenant FX margin configs.
type TenantFXMarginAudit struct {
	ID             string                 `json:"id"`
	MarginID       string                 `json:"margin_id"`
	TenantID       string                 `json:"tenant_id"`
	Action         string                 `json:"action"` // "create", "update", "delete"
	OldValues      map[string]interface{} `json:"old_values,omitempty"`
	NewValues      map[string]interface{} `json:"new_values,omitempty"`
	ActorPrincipal string                 `json:"actor_principal"`
	ActorIP        string                 `json:"actor_ip,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
}
