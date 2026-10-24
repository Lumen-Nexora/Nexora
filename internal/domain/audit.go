package domain

import (
	"strings"
	"time"
)

type AuditEvent struct {
	ID           string                 `json:"id"`
	TenantID     string                 `json:"tenant_id"`
	Mode         Mode                   `json:"mode"`
	ActorType    string                 `json:"actor_type"` // "api_key", "user", "system"
	ActorID      string                 `json:"actor_id"`
	Action       string                 `json:"action"`
	ResourceType string                 `json:"resource_type"`
	ResourceID   string                 `json:"resource_id"`
	Metadata     map[string]interface{} `json:"metadata"`
	IPAddress    *string                `json:"ip_address,omitempty"`
	UserAgent    *string                `json:"user_agent,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`

	// PrevRowHash and RowHash carry the SHA-256 tamper-proofing chain. They
	// are hex-encoded for readability in exports and API payloads; the first
	// record of a chain has a prev_row_hash of 32 zero bytes. Empty means the
	// record predates the hash chain (legacy data) and is excluded from
	// verification.
	PrevRowHash string `json:"prev_row_hash,omitempty"`
	RowHash     string `json:"row_hash,omitempty"`
}

type AuditFilter struct {
	Action       string
	ActorID      string
	ResourceType string
	ResourceID   string
	From         *time.Time
	To           *time.Time
	Limit        int
	Offset       int
}

// RedactMetadata recursively redacts sensitive information such as passwords,
// secret keys, bearer tokens, signatures, and private keys from audit payloads.
func RedactMetadata(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	redacted := make(map[string]interface{}, len(m))
	for k, v := range m {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "secret") ||
			strings.Contains(lower, "password") ||
			strings.Contains(lower, "token") ||
			strings.Contains(lower, "private_key") ||
			strings.Contains(lower, "api_key") ||
			strings.Contains(lower, "apikey") ||
			strings.Contains(lower, "key_hash") ||
			strings.Contains(lower, "seed") ||
			strings.Contains(lower, "authorization") ||
			strings.Contains(lower, "signature") {
			redacted[k] = "[REDACTED]"
			continue
		}
		if subMap, ok := v.(map[string]interface{}); ok {
			redacted[k] = RedactMetadata(subMap)
		} else {
			redacted[k] = v
		}
	}
	return redacted
}
