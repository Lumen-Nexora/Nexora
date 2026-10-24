package webhook

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

type EventCatalogEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Example     json.RawMessage `json:"example"`
}

var eventCatalog = []EventCatalogEntry{
	{domain.EventTransferSettled, "A transfer has been confirmed on Stellar.", json.RawMessage(`{"id":"txn_123","status":"confirmed","amount":"25.00","asset":"USDC","stellar_tx_hash":"abc123"}`)},
	{domain.EventTransferFailed, "A transfer failed before settlement completed.", json.RawMessage(`{"id":"txn_123","status":"failed","amount":"25.00","asset":"USDC","failure_reason":"insufficient_balance"}`)},
	{domain.EventWalletFunded, "A wallet received funds.", json.RawMessage(`{"wallet_id":"wal_123","asset":"USDC","amount":"25.00","transaction_hash":"abc123"}`)},
	{domain.EventWalletBalanceLow, "A wallet's balance has fallen below the configured threshold.", json.RawMessage(`{"alert_id":"alert_123","wallet_id":"wal_123","asset_code":"USDC","asset_issuer":"issuer_123","balance":"50.00","threshold":"100.00","triggered_at":"2024-01-01T00:00:00Z"}`)},
	{domain.EventWalletSwept, "Deposits above the sweep threshold were moved from a custodial wallet to the tenant master address, itemised per asset.", json.RawMessage(`{"sweep_id":"7c1e","wallet_id":"wal_123","public_key":"GBXC...","destination_address":"GA5Z...","transaction_hash":"abc123","items":[{"asset_code":"USDC","issuer":"GA5Z...","amount":"250.0000000","balance_before":"250.0000000","retained":"0"},{"asset_code":"XLM","amount":"40.0000000","balance_before":"42.5000000","retained":"2.5000000"}],"fee_paid":"0.0000300"}`)},
	{"payment.completed", "A payment completed successfully.", json.RawMessage(`{"payment_id":"pay_123","status":"completed","amount":"25.00","currency":"USDC"}`)},
	{"payment.failed", "A payment could not be completed.", json.RawMessage(`{"payment_id":"pay_123","status":"failed","failure_reason":"declined"}`)},
	{"fx.quote.created", "A foreign-exchange quote was created.", json.RawMessage(`{"quote_id":"quo_123","base_asset":"USDC","quote_asset":"NGN","rate":"1500.00"}`)},
	{"settlement.completed", "A settlement completed.", json.RawMessage(`{"settlement_id":"set_123","status":"completed","transaction_hash":"abc123"}`)},
	{"batch.completed", "All transfers in a batch have finished processing.", json.RawMessage(`{"batch_id":"bat_123","status":"completed","total":3,"succeeded":3,"failed":0}`)},
	{domain.EventAPIKeyRotationReminder, "An API key is approaching its configured expiry.", json.RawMessage(`{"key_id":"key_123","expires_at":"2030-01-01T00:00:00Z"}`)},
	{domain.EventAPIKeyExpired, "An API key has expired.", json.RawMessage(`{"key_id":"key_123","expired_at":"2030-01-01T00:00:00Z"}`)},
	{domain.EventContractEventEmitted, "A Soroban contract emitted an event.", json.RawMessage(`{"id":"0000000000000000001-0000000001","contract_id":"CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE","event_type":"contract","topic":"transfer","topics":["transfer"],"data":"100","ledger":1234,"tx_hash":"abc123"}`)},
}

// ListEventCatalog returns the event contract catalogue. Search is performed
// over the event name and description, never over example payload values.
func (h *Handler) ListEventCatalog(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	entries := make([]EventCatalogEntry, 0, len(eventCatalog))
	for _, entry := range eventCatalog {
		if query == "" || strings.Contains(strings.ToLower(entry.Name), query) || strings.Contains(strings.ToLower(entry.Description), query) {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	api.JSON(w, http.StatusOK, map[string]interface{}{"events": entries})
}
