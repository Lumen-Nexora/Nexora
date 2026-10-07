package domain

import "time"

// Anchor is a SEP-compliant fiat on/off-ramp registered by home domain.
// Its endpoints and signing key are discovered by fetching and parsing the
// anchor's stellar.toml (SEP-1) rather than being hand-entered.
type Anchor struct {
	ID                  string
	HomeDomain          string
	TransferServer      string // SEP-6 TRANSFER_SERVER, empty if unsupported
	TransferServerSep24 string // SEP-24 TRANSFER_SERVER_SEP0024, empty if unsupported
	WebAuthEndpoint     string // SEP-10 WEB_AUTH_ENDPOINT
	Sep10SigningKey     string // SEP-10 SIGNING_KEY
	NetworkPassphrase   string
	SupportedAssets     []string // asset codes listed in stellar.toml CURRENCIES
	SepVersions         []int    // e.g. [1, 6, 10, 24]
	RegisteredAt        time.Time
}

const (
	AnchorTxTypeDeposit    = "deposit"
	AnchorTxTypeWithdrawal = "withdrawal"
)

// AnchorTransaction is Nexora's own ledger record of a deposit/withdrawal
// initiated against a registered anchor. Status mirrors the anchor's SEP-6/24
// transaction status verbatim (e.g. "pending_user_transfer_start", "completed").
type AnchorTransaction struct {
	ID           string
	UserID       *string
	WalletID     string
	AnchorID     string
	ExternalTxID string
	Asset        string
	Amount       string
	Type         string // deposit | withdrawal
	Status       string
	CreatedAt    time.Time
	CompletedAt  *time.Time
}
