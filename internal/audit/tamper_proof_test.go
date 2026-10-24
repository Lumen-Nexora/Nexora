package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// fakeChainStore is an in-memory ChainSource standing in for the database.
type fakeChainStore struct {
	rows []ChainRow
	err  error
}

func (f *fakeChainStore) ScanChain(_ context.Context, fn func(ChainRow) error) error {
	if f.err != nil {
		return f.err
	}
	for _, row := range f.rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

func chainEvent(i int, tenant string) *domain.AuditEvent {
	return &domain.AuditEvent{
		ID:           fmt.Sprintf("evt-%d", i),
		TenantID:     tenant,
		Mode:         domain.ModeLive,
		ActorType:    "user",
		ActorID:      "user-1",
		Action:       fmt.Sprintf("transfer.created.%d", i),
		ResourceType: "transfer",
		ResourceID:   fmt.Sprintf("tx-%d", i),
		Metadata:     map[string]interface{}{"amount": "10.00", "seq": i},
		CreatedAt:    time.Date(2026, 1, 2, 3, i, 0, 0, time.UTC).Truncate(time.Microsecond),
	}
}

// chainRows links events into a stored chain, exactly the way TamperProofRepo
// does inside its insert transaction.
func chainRows(events []*domain.AuditEvent) []ChainRow {
	rows := make([]ChainRow, 0, len(events))
	prev := GenesisRowHash()
	for _, e := range events {
		h := ComputeRowHash(prev, e)
		e.PrevRowHash = HashHex(prev)
		e.RowHash = HashHex(h)
		rows = append(rows, ChainRow{Event: e, PrevHash: prev, RowHash: h})
		prev = h
	}
	return rows
}

func TestComputeRowHashMatchesTheSpec(t *testing.T) {
	prev := sha256.Sum256([]byte("previous-row"))
	e := chainEvent(0, "t1")

	want := sha256.Sum256(append(append(prev[:], RowData(e)...), []byte(e.CreatedAt.UTC().Format(time.RFC3339Nano))...))
	if got := ComputeRowHash(prev, e); got != want {
		t.Error("ComputeRowHash must be sha256(prev_row_hash | row_data | created_at)")
	}
}

func TestRowHashCoversEveryStoredField(t *testing.T) {
	prev := GenesisRowHash()
	base := ComputeRowHash(prev, chainEvent(0, "t1"))

	mutations := map[string]func(*domain.AuditEvent){
		"id":            func(e *domain.AuditEvent) { e.ID = "evt-forged" },
		"tenant_id":     func(e *domain.AuditEvent) { e.TenantID = "t-other" },
		"mode":          func(e *domain.AuditEvent) { e.Mode = domain.ModeTest },
		"actor_type":    func(e *domain.AuditEvent) { e.ActorType = "api_key" },
		"actor_id":      func(e *domain.AuditEvent) { e.ActorID = "attacker" },
		"action":        func(e *domain.AuditEvent) { e.Action = "transfer.created" },
		"resource_type": func(e *domain.AuditEvent) { e.ResourceType = "wallet" },
		"resource_id":   func(e *domain.AuditEvent) { e.ResourceID = "tx-x" },
		"metadata":      func(e *domain.AuditEvent) { e.Metadata = map[string]interface{}{"amount": "999.00"} },
		"ip_address":    func(e *domain.AuditEvent) { ip := "203.0.113.9"; e.IPAddress = &ip },
		"user_agent":    func(e *domain.AuditEvent) { ua := "curl/9"; e.UserAgent = &ua },
		"created_at": func(e *domain.AuditEvent) {
			e.CreatedAt = e.CreatedAt.Add(time.Second)
		},
	}
	for name, mutate := range mutations {
		e := chainEvent(0, "t1")
		mutate(e)
		if got := ComputeRowHash(prev, e); got == base {
			t.Errorf("mutating %s must invalidate the row hash", name)
		}
	}
}

func TestRowDataIsStableAcrossAJSONRoundTrip(t *testing.T) {
	e := chainEvent(0, "t1")
	e.IPAddress = strPtr("198.51.100.7")
	e.UserAgent = strPtr("nexora-sdk/1.0")

	// Simulate storing the metadata as JSONB and scanning it back.
	var metaBytes []byte
	metaBytes, _ = json.Marshal(e.Metadata)
	var scannedMeta map[string]interface{}
	if err := json.Unmarshal(metaBytes, &scannedMeta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	scanned := *e
	scanned.Metadata = scannedMeta

	if string(RowData(e)) != string(RowData(&scanned)) {
		t.Error("RowData must serialize identically before and after a database round trip")
	}
}

func strPtr(s string) *string { return &s }

func TestVerifyChainPassesAnIntactChain(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1"), chainEvent(2, "t1")}
	rows := chainRows(events)
	store := &fakeChainStore{rows: rows}

	report, err := VerifyChain(context.Background(), store)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !report.Verified {
		t.Fatalf("intact chain reported anomalies: %+v", report.Anomalies)
	}
	if report.RecordCount != 3 {
		t.Errorf("record count = %d, want 3", report.RecordCount)
	}
	if want := HashHex(rows[2].RowHash); report.Head != want {
		t.Errorf("head = %s, want the last row's hash %s", report.Head, want)
	}
	if report.Genesis != HashHex(GenesisRowHash()) {
		t.Errorf("genesis = %s, want %s", report.Genesis, HashHex(GenesisRowHash()))
	}
}

func TestVerifyChainDetectsSimulatedRowModification(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1"), chainEvent(2, "t1")}
	rows := chainRows(events)

	// Simulate a retroactive UPDATE: rewrite a stored action without touching
	// any hash columns.
	rows[1].Event.Action = "transfer.created-forged"
	store := &fakeChainStore{rows: rows}

	report, err := VerifyChain(context.Background(), store)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if report.Verified {
		t.Fatal("a modified record must break verification")
	}
	found := false
	for _, a := range report.Anomalies {
		if a.Kind == AnomalyModified && a.RecordID == "evt-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing modified_record anomaly for evt-1, got %+v", report.Anomalies)
	}
}

func TestVerifyChainDetectsSimulatedRowInsertion(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1"), chainEvent(2, "t1")}
	rows := chainRows(events)

	// Simulate an attacker inserting a forged record after the first row. The
	// forged row must be plausibly self-hashed for the attack to stand a
	// chance, but the original second row still points at the genuine first
	// row's hash; the insertion therefore shows up as a broken link on the
	// displaced successor. (An attacker who relinks the whole suffix is
	// beyond a hash chain — the daily on-chain Merkle attestation covers it.)
	forged := chainEvent(1, "t1")
	forged.ID = "evt-forged"
	forged.Action = "transfer.created-attacker"
	forgedHash := ComputeRowHash(rows[0].RowHash, forged)
	forged.PrevRowHash = HashHex(rows[0].RowHash)
	forged.RowHash = HashHex(forgedHash)

	store := &fakeChainStore{
		rows: []ChainRow{
			rows[0],
			{Event: forged, PrevHash: rows[0].RowHash, RowHash: forgedHash},
			rows[1],
			rows[2],
		},
	}

	report, err := VerifyChain(context.Background(), store)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if report.Verified {
		t.Fatal("an inserted record must break verification")
	}
	var broken bool
	for _, a := range report.Anomalies {
		if a.Kind == AnomalyBrokenLink && a.RecordID == "evt-1" {
			broken = true
		}
	}
	if !broken {
		t.Errorf("expected a broken link on the displaced row evt-1, got %+v", report.Anomalies)
	}
}

func TestVerifyChainDetectsSimulatedRowDeletion(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1"), chainEvent(2, "t1")}
	rows := chainRows(events)

	// Simulate a DELETE of the middle row: the surviving successor still
	// carries the deleted row's hash in prev_row_hash.
	store := &fakeChainStore{rows: []ChainRow{rows[0], rows[2]}}

	report, err := VerifyChain(context.Background(), store)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if report.Verified {
		t.Fatal("a deleted record must break verification")
	}
	found := false
	for _, a := range report.Anomalies {
		if a.Kind == AnomalyBrokenLink && a.RecordID == "evt-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing broken_hash_link anomaly for evt-2, got %+v", report.Anomalies)
	}
}

func TestVerifyChainReportsSourceErrors(t *testing.T) {
	store := &fakeChainStore{err: fmt.Errorf("connection reset")}
	if _, err := VerifyChain(context.Background(), store); err == nil {
		t.Error("expected the scan error to surface")
	}
}

func TestChainBundleRoundTripsAndVerifies(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1")}
	store := &fakeChainStore{rows: chainRows(events)}

	bundle, err := ExportChainBundle(context.Background(), store)
	if err != nil {
		t.Fatalf("ExportChainBundle: %v", err)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}

	report, err := VerifyChainBundle(data)
	if err != nil {
		t.Fatalf("VerifyChainBundle: %v", err)
	}
	if !report.Verified {
		t.Fatalf("exported bundle must verify: %+v", report.Anomalies)
	}
	if report.RecordCount != 2 {
		t.Errorf("record count = %d, want 2", report.RecordCount)
	}
	if report.Head != bundle.Head {
		t.Errorf("head = %s, want %s", report.Head, bundle.Head)
	}
}

func TestVerifiedBundleDetectsAnEditedRecord(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1")}
	store := &fakeChainStore{rows: chainRows(events)}

	bundle, err := ExportChainBundle(context.Background(), store)
	if err != nil {
		t.Fatalf("ExportChainBundle: %v", err)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}

	// The recipient edits a record before checking it in as evidence.
	var edited ChainBundle
	if err := json.Unmarshal(data, &edited); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	edited.Records[1].Action = "transfer.created-forged"
	tampered, err := json.Marshal(edited)
	if err != nil {
		t.Fatalf("marshal edited bundle: %v", err)
	}

	report, err := VerifyChainBundle(tampered)
	if err != nil {
		t.Fatalf("VerifyChainBundle: %v", err)
	}
	if report.Verified {
		t.Fatal("an edited bundle record must fail verification")
	}
	found := false
	for _, a := range report.Anomalies {
		if a.Kind == AnomalyModified && a.RecordID == "evt-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing modified_record anomaly for evt-1, got %+v", report.Anomalies)
	}
}

func TestVerifyChainBundleRejectsMalformedHashesAndAlgorithms(t *testing.T) {
	if _, err := VerifyChainBundle([]byte(`{"schema":"x","algorithm":"sha1(...)","records":[]}`)); err == nil {
		t.Error("expected an error for an unsupported algorithm")
	}
	events := []*domain.AuditEvent{chainEvent(0, "t1")}
	store := &fakeChainStore{rows: chainRows(events)}
	bundle, err := ExportChainBundle(context.Background(), store)
	if err != nil {
		t.Fatalf("ExportChainBundle: %v", err)
	}
	bundle.Records[0].RowHash = "not-hex"
	data, _ := json.Marshal(bundle)
	if _, err := VerifyChainBundle(data); err == nil {
		t.Error("expected an error for a malformed row hash")
	}
}

type recordingAlerter struct {
	alerts chan *IntegrityReport
}

func (r *recordingAlerter) AlertChainTamper(_ context.Context, report *IntegrityReport) {
	r.alerts <- report
}

func TestChainVerifierRaisesAnImmediateAlertOnTampering(t *testing.T) {
	events := []*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1")}
	rows := chainRows(events)
	rows[0].Event.Action = "transfer.created-forged"

	alerter := &recordingAlerter{alerts: make(chan *IntegrityReport, 8)}
	verifier := NewChainVerifier(&fakeChainStore{rows: rows}, alerter, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go verifier.Run(ctx)

	select {
	case report := <-alerter.alerts:
		if report.Verified {
			t.Error("the alerter must receive an unverified report")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tamper alert raised within the startup pass")
	}
}

func TestChainVerifierStaysSilentOnAHealthyChain(t *testing.T) {
	rows := chainRows([]*domain.AuditEvent{chainEvent(0, "t1"), chainEvent(1, "t1")})

	alerter := &recordingAlerter{alerts: make(chan *IntegrityReport, 8)}
	verifier := NewChainVerifier(&fakeChainStore{rows: rows}, alerter, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go verifier.Run(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()

	for {
		select {
		case report := <-alerter.alerts:
			t.Errorf("healthy chain raised an alert: %+v", report.Summary())
			continue
		default:
		}
		break
	}
}
