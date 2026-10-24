package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/alerting"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/rs/zerolog/log"
)

// ChainAlgorithm names the hash-chain formula so exported bundles can be
// re-verified by external tooling.
const ChainAlgorithm = "sha256(prev_row_hash | row_data | created_at)"

// bundleSchema tags exported audit bundles.
const bundleSchema = "nexora.audit-chain/v1"

// GenesisRowHash is the previous-row hash used by the first record of a
// chain: 32 zero bytes.
func GenesisRowHash() [32]byte { return [32]byte{} }

// HashHex renders a row hash as the lowercase hex form used on the wire.
func HashHex(h [32]byte) string { return hex.EncodeToString(h[:]) }

// ParseHashHex parses a hex-encoded row hash.
func ParseHashHex(s string) ([32]byte, error) {
	var h [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("decode row hash: %w", err)
	}
	if len(raw) != len(h) {
		return h, fmt.Errorf("row hash is %d bytes, want %d", len(raw), len(h))
	}
	copy(h[:], raw)
	return h, nil
}

// metadataJSON serializes event metadata deterministically. An empty map and
// a nil map both encode as {}, so a record hashes identically whether the
// metadata round-tripped through the database or not.
func metadataJSON(e *domain.AuditEvent) []byte {
	if len(e.Metadata) == 0 {
		return []byte("{}")
	}
	encoded, err := json.Marshal(e.Metadata)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// canonicalRow is the fixed-shape serialization hashed into every row hash.
// Struct field order fixes the byte layout, and encoding/json sorts map keys,
// so the same event always hashes the same way before and after a database
// round trip.
type canonicalRow struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	Mode         string          `json:"mode"`
	ActorType    string          `json:"actor_type"`
	ActorID      string          `json:"actor_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Metadata     json.RawMessage `json:"metadata"`
	IPAddress    string          `json:"ip_address"`
	UserAgent    string          `json:"user_agent"`
}

// RowData returns the canonical row_data bytes for an audit event. Every
// stored field is covered, so changing any of them invalidates the hash.
func RowData(e *domain.AuditEvent) []byte {
	var ip, ua string
	if e.IPAddress != nil {
		ip = *e.IPAddress
	}
	if e.UserAgent != nil {
		ua = *e.UserAgent
	}
	row := canonicalRow{
		ID:           e.ID,
		TenantID:     e.TenantID,
		Mode:         string(e.Mode),
		ActorType:    e.ActorType,
		ActorID:      e.ActorID,
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		Metadata:     metadataJSON(e),
		IPAddress:    ip,
		UserAgent:    ua,
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		// The struct contains only strings and a RawMessage; json.Marshal
		// cannot fail on it. Fall back to a stable placeholder anyway rather
		// than panicking an audit write.
		return []byte("{}")
	}
	return encoded
}

// ComputeRowHash produces the row digest per the acceptance criteria:
// SHA-256(prev_row_hash + row_data + timestamp). The timestamp is the
// event's created_at rendered in UTC RFC 3339 with nanoseconds, appended
// explicitly after the canonical row_data.
func ComputeRowHash(prev [32]byte, e *domain.AuditEvent) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	h.Write(RowData(e))
	h.Write([]byte(e.CreatedAt.UTC().Format(time.RFC3339Nano)))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ChainRow is one record of the stored chain: the audit event plus the two
// hashes persisted beside it.
type ChainRow struct {
	Event    *domain.AuditEvent
	PrevHash [32]byte
	RowHash  [32]byte
}

// ChainSource iterates the stored chain in append order, oldest first.
// Tamper-proof repositories implement it; tests replace it with an in-memory
// fake.
type ChainSource interface {
	ScanChain(ctx context.Context, fn func(ChainRow) error) error
}

// Anomaly kinds reported by the verifier.
const (
	AnomalyModified   = "modified_record"
	AnomalyBrokenLink = "broken_hash_link"
)

// ChainAnomaly is a single integrity failure found while walking the chain.
type ChainAnomaly struct {
	Kind     string `json:"kind"`
	RecordID string `json:"record_id"`
	Detail   string `json:"detail"`
}

// IntegrityReport summarizes one pass of chain verification.
type IntegrityReport struct {
	Verified    bool           `json:"verified"`
	RecordCount int            `json:"record_count"`
	Genesis     string         `json:"genesis"`
	Head        string         `json:"head"`
	Anomalies   []ChainAnomaly `json:"anomalies,omitempty"`
}

// Summary renders a report as a short, single-line description for pagers.
func (r *IntegrityReport) Summary() string {
	if r.Verified {
		return fmt.Sprintf("audit chain verified: %d records, head %s", r.RecordCount, r.Head)
	}
	summary := fmt.Sprintf("audit chain tampering detected: %d record(s) affected across %d records",
		len(r.Anomalies), r.RecordCount)
	const maxListed = 3
	for i, a := range r.Anomalies {
		if i == maxListed {
			summary += fmt.Sprintf("; and %d more", len(r.Anomalies)-i)
			break
		}
		summary += fmt.Sprintf("; %s[%s]: %s", a.Kind, a.RecordID, a.Detail)
	}
	return summary
}

// Anomaly set by both live records and bundle records.
const anomalyDetailLink = "prev_row_hash does not match the preceding row's hash; a record was inserted or deleted"
const anomalyDetailModified = "recomputed row hash does not match the stored row_hash; the record was modified after write"

// verifyRows re-walks a list of chain records in append order, flagging
// modifications, insertions, and deletions. The walk is shared between live
// verification and exported-bundle verification.
//
// Attackers who relink an entire chain suffix cannot be caught by the chain
// alone; the on-chain Merkle attestation daemon (onchain_attestation.go)
// anchors each day's root externally for that reason.
func verifyRows(rows []ChainRow) *IntegrityReport {
	report := &IntegrityReport{RecordCount: len(rows), Genesis: HashHex(GenesisRowHash())}
	expected := GenesisRowHash()
	for _, row := range rows {
		if row.PrevHash != expected {
			report.Anomalies = append(report.Anomalies, ChainAnomaly{
				Kind:     AnomalyBrokenLink,
				RecordID: row.Event.ID,
				Detail:   anomalyDetailLink,
			})
		}
		if got := ComputeRowHash(row.PrevHash, row.Event); got != row.RowHash {
			report.Anomalies = append(report.Anomalies, ChainAnomaly{
				Kind:     AnomalyModified,
				RecordID: row.Event.ID,
				Detail:   anomalyDetailModified,
			})
		}
		expected = row.RowHash
		report.Head = HashHex(row.RowHash)
	}
	report.Verified = len(report.Anomalies) == 0
	return report
}

// VerifyChain recomputes every row hash in append order over the stored
// chain and reports modifications, insertions, and deletions.
func VerifyChain(ctx context.Context, source ChainSource) (*IntegrityReport, error) {
	var rows []ChainRow
	if err := source.ScanChain(ctx, func(row ChainRow) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan audit chain: %w", err)
	}
	return verifyRows(rows), nil
}

// ChainRecord is one record of an exported, externally verifiable bundle.
type ChainRecord struct {
	domain.AuditEvent
	PrevRowHash string `json:"prev_row_hash"`
	RowHash     string `json:"row_hash"`
}

// ChainBundle is the export format handed to external regulatory
// inspections. Any auditor can recompute the chain from the records alone
// and confirm head matches the published hash.
type ChainBundle struct {
	Schema      string        `json:"schema"`
	Algorithm   string        `json:"algorithm"`
	GeneratedAt time.Time     `json:"generated_at"`
	RecordCount int           `json:"record_count"`
	Genesis     string        `json:"genesis"`
	Head        string        `json:"head"`
	Records     []ChainRecord `json:"records"`
}

// ExportChainBundle walks the whole chain and serializes it as a verifiable
// JSON bundle for regulatory inspection.
func ExportChainBundle(ctx context.Context, source ChainSource) (*ChainBundle, error) {
	bundle := &ChainBundle{
		Schema:    bundleSchema,
		Algorithm: ChainAlgorithm,
		Genesis:   HashHex(GenesisRowHash()),
	}
	err := source.ScanChain(ctx, func(row ChainRow) error {
		bundle.Records = append(bundle.Records, ChainRecord{
			AuditEvent:  *row.Event,
			PrevRowHash: HashHex(row.PrevHash),
			RowHash:     HashHex(row.RowHash),
		})
		bundle.Head = HashHex(row.RowHash)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan audit chain for export: %w", err)
	}
	bundle.RecordCount = len(bundle.Records)
	bundle.GeneratedAt = time.Now().UTC()
	return bundle, nil
}

// VerifyChainBundle re-verifies an exported bundle. It recomputes every row
// hash and walks the link list exactly like VerifyChain, so an inspector can
// detect a bundle whose records were edited after export.
func VerifyChainBundle(data []byte) (*IntegrityReport, error) {
	var bundle ChainBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("parse audit bundle: %w", err)
	}
	if bundle.Algorithm != ChainAlgorithm {
		return nil, fmt.Errorf("unsupported bundle algorithm %q", bundle.Algorithm)
	}

	rows := make([]ChainRow, 0, len(bundle.Records))
	for i := range bundle.Records {
		prev, err := ParseHashHex(bundle.Records[i].PrevRowHash)
		if err != nil {
			return nil, fmt.Errorf("record %d prev_row_hash: %w", i, err)
		}
		rowHash, err := ParseHashHex(bundle.Records[i].RowHash)
		if err != nil {
			return nil, fmt.Errorf("record %d row_hash: %w", i, err)
		}
		event := bundle.Records[i].AuditEvent
		rows = append(rows, ChainRow{Event: &event, PrevHash: prev, RowHash: rowHash})
	}

	report := verifyRows(rows)
	if report.Head != bundle.Head {
		report.Verified = false
		report.Anomalies = append(report.Anomalies, ChainAnomaly{
			Detail: "bundle head does not match the last record's row_hash",
		})
	}
	return report, nil
}

// TamperAlerter pages security operations when the verifier finds tampering.
type TamperAlerter interface {
	AlertChainTamper(ctx context.Context, report *IntegrityReport)
}

// LogAlerter is the fallback alerter: it logs tampering at error level. The
// worker wires an AlertClientAlerter instead so the notification reaches an
// actual on-call channel.
type LogAlerter struct{}

func (LogAlerter) AlertChainTamper(_ context.Context, report *IntegrityReport) {
	log.Error().
		Str("head", report.Head).
		Int("record_count", report.RecordCount).
		Msg(report.Summary())
}

// AlertClientAlerter raises a critical security-operations alert through the
// existing alerting webhook client.
type AlertClientAlerter struct {
	client *alerting.Client
}

func NewAlertClientAlerter(client *alerting.Client) *AlertClientAlerter {
	return &AlertClientAlerter{client: client}
}

func (a *AlertClientAlerter) AlertChainTamper(ctx context.Context, report *IntegrityReport) {
	a.client.Send(ctx, alerting.LevelCritical, "Audit log tamper detected", report.Summary())
}

// ChainVerifier periodically re-verifies the audit log hash chain in the
// background and dispatches an alert the moment tampering is detected.
type ChainVerifier struct {
	source   ChainSource
	alerter  TamperAlerter
	interval time.Duration
}

// defaultVerifyInterval audits the chain every fifteen minutes when the
// operator does not configure an interval.
const defaultVerifyInterval = 15 * time.Minute

func NewChainVerifier(source ChainSource, alerter TamperAlerter, interval time.Duration) *ChainVerifier {
	if interval <= 0 {
		interval = defaultVerifyInterval
	}
	if alerter == nil {
		alerter = LogAlerter{}
	}
	return &ChainVerifier{source: source, alerter: alerter, interval: interval}
}

// Verify runs exactly one verification pass.
func (v *ChainVerifier) Verify(ctx context.Context) (*IntegrityReport, error) {
	return VerifyChain(ctx, v.source)
}

// Run verifies immediately at startup and then on every interval until ctx
// is cancelled. Healthy passes log at debug level; tampered passes alert
// security operations on discovery rather than waiting for the next tick.
func (v *ChainVerifier) Run(ctx context.Context) {
	ticker := time.NewTicker(v.interval)
	defer ticker.Stop()

	for {
		report, err := v.Verify(ctx)
		switch {
		case err != nil:
			log.Error().Err(err).Msg("audit chain verification failed")
		case report.Verified:
			log.Debug().Int("record_count", report.RecordCount).Msg("audit chain verified")
		default:
			log.Error().Msg(report.Summary())
			v.alerter.AlertChainTamper(ctx, report)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
